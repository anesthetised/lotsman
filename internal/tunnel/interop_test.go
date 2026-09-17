package tunnel

import (
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"

	wgconn "golang.zx2c4.com/wireguard/conn"
	wgdevice "golang.zx2c4.com/wireguard/device"
	wgnetstack "golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/anesthetised/lotsman/internal/awgconf"
)

// A provider that runs stock WireGuard, not AmneziaWG, must work as an
// upstream: with no obfuscation parameters amneziawg-go speaks plain WireGuard.
func TestPlainWireGuardInterop(t *testing.T) {
	providerKey, _ := awgconf.GeneratePrivateKey()
	ourKey, _ := awgconf.GeneratePrivateKey()
	providerAddr, ourAddr := netip.MustParseAddr("10.200.0.1"), netip.MustParseAddr("10.200.0.2")

	// The provider: upstream wireguard-go, untouched.
	ptun, pnet, err := wgnetstack.CreateNetTUN([]netip.Addr{providerAddr}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	provider := wgdevice.NewDevice(ptun, wgconn.NewDefaultBind(), wgdevice.NewLogger(wgdevice.LogLevelError, "wg: "))
	defer provider.Close()
	if err := provider.IpcSet("private_key=" + providerKey.Hex() + "\nlisten_port=51998\n" +
		"public_key=" + ourKey.Public().Hex() + "\nallowed_ip=10.200.0.2/32\n"); err != nil {
		t.Fatal(err)
	}
	if err := provider.Up(); err != nil {
		t.Fatal(err)
	}
	ln, err := pnet.ListenTCPAddrPort(netip.AddrPortFrom(providerAddr, 80))
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

	// Our side, configured from the plain WireGuard .conf the provider would hand out.
	conf := `[Interface]
PrivateKey = ` + ourKey.String() + `
Address = 10.200.0.2/32

[Peer]
PublicKey = ` + providerKey.Public().String() + `
Endpoint = 127.0.0.1:51998
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 5
`
	cfg, err := awgconf.Parse(strings.NewReader(conf))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interface.Params != (awgconf.Params{}) {
		t.Fatalf("plain config yielded obfuscation params: %+v", cfg.Interface.Params)
	}
	ours, ournet := newDevice(t, "lotsman-upstream", ourAddr)
	if err := ours.Configure(cfg.UAPI()); err != nil {
		t.Fatal(err)
	}
	if err := ours.Up(); err != nil {
		t.Fatal(err)
	}

	c, err := ournet.DialTCPAddrPort(netip.AddrPortFrom(providerAddr, 80))
	if err != nil {
		t.Fatalf("dial through plain WireGuard provider: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	payload := strings.Repeat("y", 2000)
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("echo through plain WireGuard provider: %v", err)
	}
	peers, err := ours.Peers()
	if err != nil || len(peers) != 1 || peers[0].LastHandshake.IsZero() {
		t.Errorf("no handshake recorded: %+v, %v", peers, err)
	}
}
