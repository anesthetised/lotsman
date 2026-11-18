package proxy

import (
	"bytes"
	"encoding/base64"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"
)

const (
	testUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"
	testPBK  = "wsxdeEB2O7Mqa1ny8bxMHOMyTzvWvvVQXqhNBNPMn2E" // 32 bytes, unpadded base64url
)

func vlessLink(query string) string {
	return "vless://" + testUUID + "@vpn.example.org:443?" + query + "#My%20server"
}

const (
	baseQuery = "security=reality&pbk=" + testPBK + "&sni=www.example.com&flow=xtls-rprx-vision"
	grpcQuery = "security=reality&pbk=" + testPBK + "&sni=www.example.com&type=grpc"
)

func TestParseVLESS(t *testing.T) {
	pqv := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 1952))
	for _, tc := range []struct {
		name, link string
		check      func(t *testing.T, k *vlessKey)
	}{
		{"minimal", vlessLink(baseQuery), func(t *testing.T, k *vlessKey) {
			if k.Server() != "vpn.example.org:443" || k.id != testUUID || k.flow != flowVision || k.serverName != "www.example.com" {
				t.Errorf("parsed %+v", k)
			}
			// Xray's defaults for what the link leaves out.
			if k.fingerprint != "chrome" || k.spiderX != "/" || !bytes.Equal(k.shortID, make([]byte, 8)) || k.mldsa65 != nil {
				t.Errorf("defaults %+v", k)
			}
			if len(k.publicKey) != 32 || len(k.spiderY) != 10 {
				t.Errorf("key %d bytes, spiderY %v", len(k.publicKey), k.spiderY)
			}
			if got := k.String(); got != "vless reality xtls-rprx-vision, sni www.example.com, fp chrome" {
				t.Errorf("String = %q", got)
			}
			if k.network != "tcp" {
				t.Errorf("network %q", k.network)
			}
		}},
		{"grpc as a provider exports it", "vless://" + testUUID + "@192.0.2.1:443?type=grpc&security=reality&encryption=none&sni=www.example.com" +
			"&fp=firefox&pbk=" + testPBK + "&sid=6ba85179e30d4fc2&alpn=h2&serviceName=my-service#x",
			func(t *testing.T, k *vlessKey) {
				if k.network != "grpc" || k.flow != "" || k.serviceName != "my-service" || k.multiMode || k.authority != "" || k.fingerprint != "firefox" {
					t.Errorf("parsed %+v", k)
				}
				if got := k.String(); got != "vless reality grpc, sni www.example.com, fp firefox" {
					t.Errorf("String = %q", got)
				}
			}},
		{"grpc multi, custom path, authority", vlessLink(strings.Replace(grpcQuery, "type=grpc", "type=GRPC", 1) +
			"&mode=multi&serviceName=" + url.QueryEscape("/a b/c/tun") + "&authority=cdn.example.net&alpn=h2,http/1.1"),
			func(t *testing.T, k *vlessKey) {
				if k.network != "grpc" || !k.multiMode || k.serviceName != "/a b/c/tun" || k.authority != "cdn.example.net" {
					t.Errorf("parsed %+v", k)
				}
				// The service name may be a path secret.
				if got := k.String(); got != "vless reality grpc multi, sni www.example.com, fp chrome" {
					t.Errorf("String = %q", got)
				}
			}},
		{"grpc with xray's defaults", vlessLink(grpcQuery + "&mode=gun&flow=&serviceName="), func(t *testing.T, k *vlessKey) {
			if k.network != "grpc" || k.multiMode || k.serviceName != "" || k.flow != "" || len(k.spiderY) != 10 {
				t.Errorf("parsed %+v", k)
			}
		}},
		{"alpn on tcp, ignored", vlessLink(baseQuery + "&alpn=h2"), func(t *testing.T, k *vlessKey) {
			if k.network != "tcp" || k.flow != flowVision {
				t.Errorf("parsed %+v", k)
			}
		}},
		{"panel export", "vless://" + testUUID + "@192.0.2.1:8443/?type=tcp&encryption=none&security=reality&pbk=" + testPBK +
			"&fp=Firefox&sni=www.example.com&sid=6ba85179e30d4fc2&spx=%2Fpath%3Fp%3D10-20%26c%3D3%26x%3D1&flow=xtls-rprx-vision-udp443&headerType=none&host=&path=#x",
			func(t *testing.T, k *vlessKey) {
				if k.Server() != "192.0.2.1:8443" || k.flow != flowVisionUDP443 || k.fingerprint != "firefox" {
					t.Errorf("parsed %+v", k)
				}
				if !bytes.Equal(k.shortID, []byte{0x6b, 0xa8, 0x51, 0x79, 0xe3, 0x0d, 0x4f, 0xc2}) {
					t.Errorf("sid % x", k.shortID)
				}
				if k.spiderX != "/path?x=1" || !slices.Equal(k.spiderY, []int64{10, 20, 3, 3, 0, 0, 0, 0, 0, 0}) {
					t.Errorf("spider %q %v", k.spiderX, k.spiderY)
				}
			}},
		{"short sid, raw type, IPv6", "VLESS://" + testUUID + "@[2001:db8::1]:443?" + baseQuery + "&sid=ab&type=raw&pqv=" + pqv,
			func(t *testing.T, k *vlessKey) {
				if k.Server() != "[2001:db8::1]:443" || !bytes.Equal(k.shortID, []byte{0xab, 0, 0, 0, 0, 0, 0, 0}) || len(k.mldsa65) != 1952 {
					t.Errorf("parsed %+v", k)
				}
			}},
		{"xray's short ID mapping", "vless://my-password@vpn.example.org:443?" + baseQuery, func(t *testing.T, k *vlessKey) {
			// Xray maps 1 to 30 characters to a version 5 UUID.
			if len(k.id) != 36 || k.id[14] != '5' {
				t.Errorf("id %q", k.id)
			}
		}},
		{"spider ranges as xray reads them", vlessLink(baseQuery + "&spx=" + url.QueryEscape("/?p=5-&c=-4&t=1-2-3&i=x&r=7")), func(t *testing.T, k *vlessKey) {
			if want := []int64{5, 0, 0, 4, 1, 2, 0, 0, 7, 7}; !slices.Equal(k.spiderY, want) || k.spiderX != "/" {
				t.Errorf("spider %q %v, want / %v", k.spiderX, k.spiderY, want)
			}
		}},
		{"random fingerprints are xray's", vlessLink(baseQuery + "&fp=randomized"), func(t *testing.T, k *vlessKey) {
			if k.fingerprint != "randomized" {
				t.Errorf("fp %q", k.fingerprint)
			}
		}},
		{"raw display name", vlessLink(baseQuery) + " 100% #fast", func(t *testing.T, k *vlessKey) {
			if k.serverName != "www.example.com" {
				t.Errorf("parsed %+v", k)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, err := ParseSpecKey(" " + tc.link + "\n")
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, k.(*vlessKey))
		})
	}
}

func TestParseVLESSErrorsHideTheSecret(t *testing.T) {
	// The secrets are the UUID and the keys; a malformed link may put them
	// anywhere.
	secrets := []string{testUUID, testPBK, "hunter2secret"}
	for _, tc := range []struct{ name, link, want string }{
		{"other scheme", "ss://" + testUUID + "@host:443", "want a vless:// link"},
		{"malformed URL", "vless://" + testUUID + "@host:443?%zz" + testPBK, "malformed"},
		{"malformed escape in user", "vless://%zz" + testUUID + "@host:443?" + baseQuery, "malformed URL"},
		{"no user", "vless://host:443?" + baseQuery, "user ID"},
		{"password", "vless://" + testUUID + ":hunter2secret@host:443?" + baseQuery, "not user:password"},
		{"not a uuid", "vless://" + testUUID + "x@host:443?" + baseQuery, "not a UUID"},
		{"long non-uuid", "vless://" + strings.Repeat("hunter2secret", 3) + "@host:443?" + baseQuery, "not a UUID"},
		{"no port", "vless://" + testUUID + "@host?" + baseQuery, "host:port"},
		{"port zero", "vless://" + testUUID + "@host:0?" + baseQuery, "port"},
		{"path", "vless://" + testUUID + "@host:443/hunter2secret?" + baseQuery, "want vless://uuid@host:port"},
		{"no security", vlessLink(strings.Replace(baseQuery, "security=reality&", "", 1)), "security is missing"},
		{"tls", vlessLink(strings.Replace(baseQuery, "security=reality", "security=tls", 1)), "only security=reality"},
		{"websocket", vlessLink(baseQuery + "&type=ws"), "only type=tcp (raw) and type=grpc"},
		{"grpc with vision", vlessLink(baseQuery + "&type=grpc"), "flow must be empty for type=grpc"},
		{"grpc with another flow", vlessLink(grpcQuery + "&flow=hunter2secret"), "flow must be empty"},
		{"grpc unknown mode", vlessLink(grpcQuery + "&mode=hunter2secret"), "mode: want gun or multi"},
		{"grpc gun lite", vlessLink(grpcQuery + "&mode=guna"), "mode: want gun or multi"},
		{"grpc without sni", vlessLink(strings.Replace(grpcQuery, "&sni=www.example.com", "", 1)), "sni is missing"},
		{"grpc without pbk", vlessLink(strings.Replace(grpcQuery, "pbk="+testPBK+"&", "", 1)), "pbk is missing"},
		{"grpc over tls", vlessLink(strings.Replace(grpcQuery, "security=reality", "security=tls", 1)), "only security=reality"},
		{"service name on tcp", vlessLink(baseQuery + "&serviceName=hunter2secret"), "serviceName applies to type=grpc only"},
		{"mode on tcp", vlessLink(baseQuery + "&mode=multi"), "mode applies to type=grpc only"},
		{"authority on tcp", vlessLink(baseQuery + "&authority=hunter2secret"), "authority applies to type=grpc only"},
		{"http header", vlessLink(baseQuery + "&headerType=http"), "only headerType=none"},
		{"mlkem encryption", vlessLink(baseQuery + "&encryption=mlkem768x25519plus.native.0rtt." + testPBK), "only encryption=none"},
		{"no flow", vlessLink(strings.Replace(baseQuery, "&flow=xtls-rprx-vision", "", 1)), "flow is missing"},
		{"other flow", vlessLink(strings.Replace(baseQuery, "xtls-rprx-vision", "xtls-rprx-direct", 1)), "unsupported flow"},
		{"no sni", vlessLink(strings.Replace(baseQuery, "&sni=www.example.com", "", 1)), "sni is missing"},
		{"no pbk", vlessLink(strings.Replace(baseQuery, "pbk="+testPBK+"&", "", 1)), "pbk is missing"},
		{"padded pbk", vlessLink(strings.Replace(baseQuery, testPBK, testPBK+"=", 1)), "pbk: want 32 bytes"},
		{"short pbk", vlessLink(strings.Replace(baseQuery, testPBK, testPBK[:40], 1)), "pbk: want 32 bytes"},
		{"long sid", vlessLink(baseQuery + "&sid=0123456789abcdef0"), "sid: at most 16"},
		{"odd sid", vlessLink(baseQuery + "&sid=abc"), "sid: want an even number"},
		{"secret sid", vlessLink(baseQuery + "&sid=hunter2secret"), "sid: want"},
		{"unsafe fp", vlessLink(baseQuery + "&fp=unsafe"), "refused by Xray"},
		{"golang fp", vlessLink(baseQuery + "&fp=HelloGolang"), "refused by Xray"},
		{"unknown fp", vlessLink(baseQuery + "&fp=hunter2secret"), "unknown fingerprint"},
		{"relative spx", vlessLink(baseQuery + "&spx=hunter2secret"), "spx: must start with /"},
		{"malformed spx", vlessLink(baseQuery + "&spx=/%25zz"), "spx: malformed"},
		{"short pqv", vlessLink(baseQuery + "&pqv=" + testPBK), "pqv: want"},
		{"twice", vlessLink(baseQuery + "&pbk=" + testPBK), "pbk given more than once"},
		{"unknown parameter", vlessLink(baseQuery + "&path=hunter2secret"), "path is not supported"},
		{"unknown grpc parameter", vlessLink(grpcQuery + "&seed=hunter2secret"), "seed is not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseVLESS(tc.link)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
			for _, s := range secrets {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("error quotes a secret: %v", err)
				}
			}
		})
	}
}

// Xray's client indexes spiderY[0..9] and dereferences the parsed spiderX
// without checks once a server presents a real certificate: a config that
// misses them panics the process (found in the spike for #86). Whatever a
// link holds, a key that parses has both.
func FuzzParseVLESS(f *testing.F) {
	f.Add(baseQuery)
	f.Add(baseQuery + "&spx=/a?p=1-2&c=3&r=-")
	f.Add(baseQuery + "&spx=/%zz")
	f.Add(baseQuery + "&sid=0&fp=random")
	f.Add(grpcQuery + "&mode=multi&serviceName=/a/b|c&authority=x")
	f.Fuzz(func(t *testing.T, query string) {
		k, err := ParseVLESS(vlessLink(query))
		if err != nil {
			return
		}
		v := k.(*vlessKey)
		if len(v.spiderY) != 10 || !strings.HasPrefix(v.spiderX, "/") || len(v.shortID) != 8 || len(v.publicKey) != 32 {
			t.Fatalf("key %+v", v)
		}
		// Vision runs over TCP only.
		if (v.network == "grpc") != (v.flow == "") {
			t.Fatalf("network %q with flow %q", v.network, v.flow)
		}
	})
}

// A VLESS link is a key for kind vless only: neither a static nor a dynamic
// Outline key reads it.
func TestVLESSIsNotAnOutlineKey(t *testing.T) {
	if _, err := ParseKey(vlessLink(baseQuery)); err == nil {
		t.Error("ParseKey read a vless:// link")
	}
	if _, err := ParseConfig(vlessLink(baseQuery)); err == nil {
		t.Error("ParseConfig read a vless:// link")
	}
	if _, err := ParseVLESS("ss://YWVzLTI1Ni1nY206eA@192.0.2.1:8388"); err == nil {
		t.Error("ParseVLESS read an ss:// key")
	}
}

func TestVLESSDialers(t *testing.T) {
	for _, query := range []string{baseQuery, grpcQuery + "&mode=multi&serviceName=x"} {
		k, err := ParseVLESS(vlessLink(query))
		if err != nil {
			t.Fatal(err)
		}
		d, err := k.Dialers(netip.MustParseAddrPort("192.0.2.1:443"))
		if err != nil {
			t.Fatal(err)
		}
		if d.Streams == nil || d.Packets == nil || d.Closer == nil {
			t.Fatalf("dialers %+v", d)
		}
		if err := d.Closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
