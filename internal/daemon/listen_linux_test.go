//go:build linux && integration

package daemon

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/tun"
	"github.com/amnezia-vpn/amneziawg-go/tun/netstack"

	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

// The real UDP sockets stay inside the disposable Linux test container. The
// fake dataplane keeps this test focused on daemon listen propagation rather
// than kernel forwarding; inner UDP echo still crosses authenticated AWG.
func TestDaemonListenAddress(t *testing.T) {
	for _, tc := range []struct {
		name, address, excluded, socket string
	}{
		{"IPv4", "127.0.0.1", "127.0.0.2", "udp 0100007F"},
		{"IPv6", "::1", "127.0.0.1", "udp6 00000000000000000000000001000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			const port = 51997
			h.cfg.Listen = netip.AddrPortFrom(netip.MustParseAddr(tc.address), port).String()
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
			if _, err := h.st.CreateUser("listen"); err != nil {
				t.Fatal(err)
			}
			allowed, err := h.st.AddPeer("listen", store.DefaultDevice, "nl", h.cfg.Subnet)
			if err != nil {
				t.Fatal(err)
			}
			excluded, err := h.st.AddPeer("listen", "excluded", "nl", h.cfg.Subnet)
			if err != nil {
				t.Fatal(err)
			}
			stop := h.run(t)
			defer stop()
			select {
			case <-h.d.Ready():
			case <-time.After(5 * time.Second):
				t.Fatal("daemon did not become ready")
			}

			var sockets []string
			for _, name := range []string{"udp", "udp6"} {
				data, err := os.ReadFile("/proc/net/" + name)
				if err != nil {
					t.Fatal(err)
				}
				for _, line := range strings.Split(string(data), "\n") {
					fields := strings.Fields(line)
					if len(fields) > 1 && strings.HasSuffix(fields[1], fmt.Sprintf(":%04X", port)) {
						sockets = append(sockets, name+" "+fields[1])
					}
				}
			}
			wantSocket := tc.socket + fmt.Sprintf(":%04X", port)
			if len(sockets) != 1 || sockets[0] != wantSocket {
				t.Errorf("daemon sockets = %v, want only %s", sockets, wantSocket)
			}

			target := netip.AddrPortFrom(Gateway(h.cfg).Addr(), 8080)
			echo, err := downNet.ListenUDPAddrPort(target)
			if err != nil {
				t.Fatal(err)
			}
			defer echo.Close()
			go func() {
				buf := make([]byte, 2048)
				for {
					n, from, err := echo.ReadFrom(buf)
					if err != nil {
						return
					}
					if _, err := echo.WriteTo(buf[:n], from); err != nil {
						return
					}
				}
			}()
			for _, attempt := range []struct {
				name, endpoint string
				peer           store.Peer
				wantReply      bool
			}{
				{"allowed", tc.address, allowed, true},
				{"excluded", tc.excluded, excluded, false},
			} {
				t.Run(attempt.name, func(t *testing.T) {
					tn, clientNet, err := netstack.CreateNetTUN([]netip.Addr{attempt.peer.IP}, nil, h.d.ClientMTU())
					if err != nil {
						t.Fatal(err)
					}
					client := tunnel.New("listen-client", tn, quiet)
					defer client.Close()
					conf := ClientConfig(h.cfg, h.d.id, attempt.peer, h.d.ClientMTU())
					conf.Peers[0].Endpoint = netip.AddrPortFrom(netip.MustParseAddr(attempt.endpoint), port).String()
					if err := client.Configure(conf.UAPI()); err != nil {
						t.Fatal(err)
					}
					if err := client.Up(); err != nil {
						t.Fatal(err)
					}
					c, err := clientNet.DialUDPAddrPort(netip.AddrPort{}, target)
					if err != nil {
						t.Fatal(err)
					}
					defer c.Close()
					c.SetReadDeadline(time.Now().Add(2 * time.Second))
					marker := "authenticated:" + attempt.name
					if _, err := c.Write([]byte(marker)); err != nil {
						t.Fatal(err)
					}
					buf := make([]byte, 128)
					n, err := c.Read(buf)
					if attempt.wantReply {
						if err != nil || string(buf[:n]) != marker {
							t.Fatalf("allowed echo = %q: %v", buf[:n], err)
						}
					} else if err == nil || !os.IsTimeout(err) {
						t.Fatalf("excluded echo = %q: %v, want timeout", buf[:n], err)
					}
					peers, err := h.d.down.Peers()
					if err != nil {
						t.Fatal(err)
					}
					for _, peer := range peers {
						if peer.PublicKey == attempt.peer.PublicKey {
							if peer.LastHandshake.IsZero() == attempt.wantReply {
								t.Errorf("handshake = %v, want authenticated = %v", peer.LastHandshake, attempt.wantReply)
							}
							return
						}
					}
					t.Fatal("authorized test peer missing from daemon")
				})
			}
		})
	}
}
