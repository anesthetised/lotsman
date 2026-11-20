package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

// awg3Keys are the AWG 3.x .conf keys; client configs carry only the shared ones.
var awg3Keys = []string{
	"HeaderProtectionKey", "RandomTrailers", "ContentPaddingAddition", "RekeyAfterTime",
	"RekeyTimeout", "RejectAfterTime", "KeepaliveTimeout", "MaxHandshakeAttempts", "DisableCookies",
}

// renderedKeys lists the AWG 3.x keys a rendered config sets.
func renderedKeys(conf string) []string {
	var out []string
	for line := range strings.SplitSeq(conf, "\n") {
		key, _, _ := strings.Cut(line, "=")
		for _, k := range awg3Keys {
			if strings.TrimSpace(key) == k {
				out = append(out, k)
			}
		}
	}
	return out
}

func TestClientConfigLevels(t *testing.T) {
	id, err := newIdentity()
	if err != nil {
		t.Fatal(err)
	}
	peerKey, _ := awgconf.GeneratePrivateKey()
	peer := store.Peer{PrivateKey: peerKey, PublicKey: peerKey.Public(), IP: netip.MustParseAddr("10.77.0.9")}
	for _, tc := range []struct {
		awg  string
		keys []string
	}{
		{"", []string{"HeaderProtectionKey", "RandomTrailers"}}, // the default is 3
		{"awg: 3\n", []string{"HeaderProtectionKey", "RandomTrailers"}},
		{"awg: 2\n", nil},
	} {
		t.Run(fmt.Sprintf("%q", tc.awg), func(t *testing.T) {
			cfg, err := config.Parse([]byte(tc.awg + "endpoint: vpn.example.org:51820\nupstreams: [{name: a, conf: /x, geo: NL, provider: acme}]\n"))
			if err != nil {
				t.Fatal(err)
			}
			text := ClientConfig(cfg, id, peer, 1380).String()
			parsed, err := awgconf.Parse(strings.NewReader(text))
			if err != nil {
				t.Fatalf("rendered client config does not parse: %v\n%s", err, text)
			}
			if got := renderedKeys(text); fmt.Sprint(got) != fmt.Sprint(tc.keys) {
				t.Errorf("AWG 3 keys = %v, want %v\n%s", got, tc.keys, text)
			}
			want := id.Shared(cfg.AWG)
			want.Jc, want.Jmin, want.Jmax, want.I = clientJunk.Jc, clientJunk.Jmin, clientJunk.Jmax, clientJunk.I
			if parsed.Interface.Params != want {
				t.Errorf("params = %+v, want %+v", parsed.Interface.Params, want)
			}
			// No client-side content padding, so the MTU is the one passed in.
			if parsed.Interface.MTU != 1380 {
				t.Errorf("MTU = %d, want 1380", parsed.Interface.MTU)
			}
		})
	}
}

func TestRestartRequiredForAWG(t *testing.T) {
	old := config.Default()
	cfg := old
	if err := restartRequired(&old, &cfg); err != nil {
		t.Fatalf("unchanged config: %v", err)
	}
	cfg.AWG = 2
	if err := restartRequired(&old, &cfg); err == nil || !strings.Contains(err.Error(), "[awg]") {
		t.Errorf("awg change: %v, want a restart error naming awg", err)
	}
}

// freeUDPPort returns a loopback port that was free a moment ago, so the
// listener avoids the default 51820 that other tests may hold.
func freeUDPPort(t *testing.T) uint16 {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return uint16(c.LocalAddr().(*net.UDPAddr).Port)
}

// The daemon configures its listener for the configured level in start, and
// a client configured from the rendered client config completes a handshake
// with it and carries TCP. Level 3 needs the header protection key and random
// trailers on both sides; a mismatch blocks the handshake (tunnel tests).
func TestDownstreamHandshakePerLevel(t *testing.T) {
	for _, level := range []int{3, 2} {
		t.Run(fmt.Sprintf("awg%d", level), func(t *testing.T) {
			h := newHarness(t)
			h.cfg.AWG = level
			port := freeUDPPort(t)
			h.cfg.Listen = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port).String()
			var downNet *netstack.Net
			h.d.newTUN = func(name string, mtu int) (tun.Device, error) {
				addr := netip.MustParseAddr("10.0.0.1")
				if name == DownstreamInterface {
					addr = Gateway(h.cfg).Addr()
				}
				tn, network, err := netstack.CreateNetTUN([]netip.Addr{addr}, nil, mtu)
				if name == DownstreamInterface {
					downNet = network
				}
				return tn, err
			}
			if _, err := h.st.CreateUser("alice"); err != nil {
				t.Fatal(err)
			}
			peer, err := h.st.AddPeer("alice", "nl", nlRoute, h.cfg.Subnet)
			if err != nil {
				t.Fatal(err)
			}
			h.run(t)
			select {
			case <-h.d.Ready():
			case <-time.After(5 * time.Second):
				t.Fatal("daemon did not become ready")
			}

			target := netip.AddrPortFrom(Gateway(h.cfg).Addr(), 8080)
			ln, err := downNet.ListenTCPAddrPort(target)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					go func() { defer c.Close(); io.Copy(c, c) }()
				}
			}()

			// What the user imports, read back as the client app would.
			text := ClientConfig(h.cfg, h.d.id, peer, h.d.ClientMTU()).String()
			conf, err := awgconf.Parse(strings.NewReader(text))
			if err != nil {
				t.Fatalf("rendered client config does not parse: %v\n%s", err, text)
			}
			p := conf.Interface.Params
			if (level == 3) != (p.HeaderProtectionKey == h.d.id.HeaderProtectionKey && p.RandomTrailers) {
				t.Fatalf("level %d client params = %+v", level, p)
			}
			conf.Peers[0].Endpoint = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port).String()

			tn, clientNet, err := netstack.CreateNetTUN([]netip.Addr{peer.IP}, nil, conf.Interface.MTU)
			if err != nil {
				t.Fatal(err)
			}
			client := tunnel.New("client", tn, quiet, nil)
			defer client.Close()
			if err := client.Configure(conf.UAPI()); err != nil {
				t.Fatal(err)
			}
			if err := client.Up(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, err := clientNet.DialContextTCPAddrPort(ctx, target)
			if err != nil {
				t.Fatalf("dial through the level %d listener: %v", level, err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			payload := strings.Repeat("z", 3000)
			if _, err := c.Write([]byte(payload)); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(c, buf); err != nil || string(buf) != payload {
				t.Fatalf("echo through the level %d listener: %v", level, err)
			}
		})
	}
}
