package daemon

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/mtu"
)

// These fixtures are only parsed and loaded; none is passed to a device.
func TestLoadUpstreamParameterBounds(t *testing.T) {
	key, err := awgconf.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range []struct {
		name, endpoint string
		payloadMax     int
	}{
		{"IPv4", "192.0.2.1:51820", 65507},
		{"IPv6", "[2001:db8::1]:51820", 65527},
	} {
		t.Run(family.name, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				mtu       int
				params    awgconf.Params
				wantError string
			}{
				{"default MTU", 0, awgconf.Params{}, ""},
				{"default MTU with padding", 0, awgconf.Params{S4: 8}, ""},
				{"minimum MTU", 68, awgconf.Params{}, ""},
				{"minimum effective MTU", 76, awgconf.Params{S4: 8}, ""},
				{"above minimum effective MTU", 77, awgconf.Params{S4: 8}, ""},
				{"below minimum effective MTU", 75, awgconf.Params{S4: 8}, "MTU minus S4 must be at least 68"},
				{"below minimum MTU", 67, awgconf.Params{}, "MTU minus S4 must be at least 68"},
				{"negative MTU", -1, awgconf.Params{}, "MTU minus S4 must be at least 68"},
				{"minimum int MTU", -int(^uint(0)>>1) - 1, awgconf.Params{}, "MTU minus S4 must be at least 68"},
				{"maximum int MTU", int(^uint(0) >> 1), awgconf.Params{}, "MTU plus S4 and transport overhead"},
				{"maximum wire MTU", family.payloadMax - 32, awgconf.Params{}, ""},
				{"maximum padded wire MTU", family.payloadMax - 40, awgconf.Params{S4: 8}, ""},
				{"below maximum padded wire MTU", family.payloadMax - 41, awgconf.Params{S4: 8}, ""},
				{"above maximum wire MTU", family.payloadMax - 31, awgconf.Params{}, "MTU plus S4 and transport overhead"},
				{"above maximum padded wire MTU", family.payloadMax - 39, awgconf.Params{S4: 8}, "MTU plus S4 and transport overhead"},
				{"nonmultiple of sixteen", 1381, awgconf.Params{S4: 8}, ""},
				{"padding exhausts default MTU", 0, awgconf.Params{S4: 1353}, "MTU minus S4 must be at least 68"},
				{"maximum fixed junk", 1420, awgconf.Params{Jc: 1, Jmin: uint32(family.payloadMax), Jmax: uint32(family.payloadMax)}, ""},
				{"exclusive upper junk", 1420, awgconf.Params{Jc: 1, Jmin: uint32(family.payloadMax), Jmax: uint32(family.payloadMax + 1)}, ""},
				{"oversized fixed junk", 1420, awgconf.Params{Jc: 1, Jmin: uint32(family.payloadMax + 1), Jmax: uint32(family.payloadMax + 1)}, "junk packet size must not exceed"},
				{"oversized junk range", 1420, awgconf.Params{Jc: 1, Jmax: uint32(family.payloadMax + 2)}, "junk packet size must not exceed"},
				{"dormant oversized junk", 1420, awgconf.Params{Jmax: ^uint32(0)}, ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					conf := &awgconf.Config{
						Interface: awgconf.Interface{PrivateKey: key, Addresses: []netip.Prefix{netip.MustParsePrefix("10.8.0.2/32")}, MTU: tc.mtu, Params: tc.params},
						Peers:     []awgconf.Peer{{PublicKey: key.Public(), Endpoint: family.endpoint, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}},
					}
					path := filepath.Join(t.TempDir(), "provider.conf")
					if err := os.WriteFile(path, []byte(conf.String()), 0o600); err != nil {
						t.Fatal(err)
					}
					up, err := LoadUpstream(context.Background(), config.Upstream{Name: "bounds", Conf: path}, config.Health{})
					if tc.wantError != "" {
						if err == nil || !strings.Contains(err.Error(), tc.wantError) {
							t.Fatalf("LoadUpstream error = %v, want %q", err, tc.wantError)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					wantMTU := tc.mtu
					if wantMTU == 0 {
						wantMTU = mtu.DefaultWireGuard
					}
					if up.MTU != wantMTU || up.Conf.Peers[0].Endpoint != family.endpoint || up.Conf.Interface.Params != tc.params {
						t.Fatalf("loaded MTU/endpoint/params = %d/%s/%+v", up.MTU, up.Conf.Peers[0].Endpoint, up.Conf.Interface.Params)
					}
				})
			}
		})
	}
}

// Fixtures are only parsed and loaded; none is passed to a device.
func TestLoadUpstreamWireSizes(t *testing.T) {
	key, err := awgconf.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range []struct {
		name, endpoint string
		payloadMax     int
	}{
		{"IPv4", "192.0.2.1:51820", 65507},
		{"IPv6", "[2001:db8::1]:51820", 65527},
	} {
		t.Run(family.name, func(t *testing.T) {
			big := strconv.Itoa(family.payloadMax)
			over := strconv.Itoa(family.payloadMax + 1)
			for _, tc := range []struct {
				name      string
				params    awgconf.Params
				wantError string
			}{
				{"S1 at limit", awgconf.Params{S1: uint16(family.payloadMax - 148)}, ""},
				{"S2 at limit", awgconf.Params{S2: uint16(family.payloadMax - 92)}, ""},
				{"S3 at limit", awgconf.Params{S3: uint16(family.payloadMax - 64)}, ""},
				{"S1 over limit", awgconf.Params{S1: uint16(family.payloadMax - 147)}, "S1 plus the 148-byte handshake initiation must not exceed"},
				{"S2 over limit", awgconf.Params{S2: uint16(family.payloadMax - 91)}, "S2 plus the 92-byte handshake response must not exceed"},
				{"S3 over limit", awgconf.Params{S3: uint16(family.payloadMax - 63)}, "S3 plus the 64-byte cookie reply must not exceed"},
				{"S1 maximum", awgconf.Params{S1: 65535}, "S1 plus"},
				{"S2 maximum", awgconf.Params{S2: 65535}, "S2 plus"},
				{"S3 maximum", awgconf.Params{S3: 65535}, "S3 plus"},
				{"I at limit", awgconf.Params{I: [5]string{"<r " + big + ">"}}, ""},
				{"I over limit", awgconf.Params{I: [5]string{"<r " + over + ">"}}, "I1 packet size must not exceed"},
				{"I sum over limit", awgconf.Params{I: [5]string{"", "<b 0xabcd><rd " + big + ">"}}, "I2 packet size must not exceed"},
				{"I large", awgconf.Params{I: [5]string{"<r 2147483647>"}}, "I1 packet size must not exceed"},
				{"I sum overflow", awgconf.Params{I: [5]string{"<r 9223372036854775807><rc 9223372036854775807>"}}, "I1 packet size must not exceed"},
				{"I negative", awgconf.Params{I: [5]string{"", "", "<dz -1>"}}, "I3 has a negative tag size -1"},
				{"inert I", awgconf.Params{I: [5]string{"ignored", "<r -1", "<r abc>", "<x 70000>"}}, ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					conf := &awgconf.Config{
						Interface: awgconf.Interface{PrivateKey: key, Addresses: []netip.Prefix{netip.MustParsePrefix("10.8.0.2/32")}, Params: tc.params},
						Peers:     []awgconf.Peer{{PublicKey: key.Public(), Endpoint: family.endpoint, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}},
					}
					path := filepath.Join(t.TempDir(), "provider.conf")
					if err := os.WriteFile(path, []byte(conf.String()), 0o600); err != nil {
						t.Fatal(err)
					}
					up, err := LoadUpstream(context.Background(), config.Upstream{Name: "wire", Conf: path}, config.Health{})
					if tc.wantError != "" {
						if err == nil || !strings.Contains(err.Error(), tc.wantError) {
							t.Fatalf("LoadUpstream error = %v, want %q", err, tc.wantError)
						}
						if up != nil {
							t.Fatal("rejected upstream was returned")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if up.Conf.Interface.Params != tc.params || up.Device != nil {
						t.Fatalf("loaded params/device = %+v/%v", up.Conf.Interface.Params, up.Device)
					}
				})
			}
		})
	}
}
