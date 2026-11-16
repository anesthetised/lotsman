package tunnel

import (
	"context"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	awg2conn "github.com/amnezia-vpn/amneziawg-go/conn"
	awg2device "github.com/amnezia-vpn/amneziawg-go/device"
	awg2netstack "github.com/amnezia-vpn/amneziawg-go/tun/netstack"
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

// serveEcho echoes every TCP connection accepted on ln.
func serveEcho(t *testing.T, ln net.Listener) {
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
}

// checkEcho sends more than one MTU over c and expects it back.
func checkEcho(t *testing.T, c net.Conn) {
	t.Helper()
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	payload := strings.Repeat("z", 3000)
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if string(buf) != payload {
		t.Fatal("echo returned different bytes")
	}
}

// uapiValue returns the first value of key in an IpcGet dump.
func uapiValue(raw, key string) string {
	for _, line := range strings.Split(raw, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// endpointOf is the loopback endpoint of a running device that listens on an
// ephemeral port, so parallel test processes do not contend for one.
func endpointOf(t *testing.T, ipcGet func() (string, error)) string {
	t.Helper()
	raw, err := ipcGet()
	if err != nil {
		t.Fatal(err)
	}
	port := uapiValue(raw, "listen_port")
	if port == "" || port == "0" {
		t.Fatalf("device has no listen port:\n%s", raw)
	}
	return "127.0.0.1:" + port
}

// A provider still on AWG 2.0 (amneziawg-go v0.2.19) must work as an upstream:
// with no AWG 3.x keys the 3.1 runtime speaks AWG 2.0 on the wire.
func TestAWG2RuntimeInterop(t *testing.T) {
	providerKey, _ := awgconf.GeneratePrivateKey()
	ourKey, _ := awgconf.GeneratePrivateKey()
	providerAddr, ourAddr := netip.MustParseAddr("10.201.0.1"), netip.MustParseAddr("10.201.0.2")

	// The provider: the AWG 2.0 runtime, configured before it starts.
	ptun, pnet, err := awg2netstack.CreateNetTUN([]netip.Addr{providerAddr}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	provider := awg2device.NewDevice(ptun, awg2conn.NewDefaultBind(), awg2device.NewLogger(awg2device.LogLevelError, "awg2: "))
	defer provider.Close()
	providerCfg := awgconf.Config{
		Interface: awgconf.Interface{PrivateKey: providerKey, Params: awg2},
		Peers:     []awgconf.Peer{{PublicKey: ourKey.Public(), AllowedIPs: []netip.Prefix{netip.PrefixFrom(ourAddr, 32)}}},
	}
	if err := provider.IpcSet(providerCfg.UAPI()); err != nil {
		t.Fatal(err)
	}
	if err := provider.Up(); err != nil {
		t.Fatal(err)
	}
	ln, err := pnet.ListenTCPAddrPort(netip.AddrPortFrom(providerAddr, 80))
	if err != nil {
		t.Fatal(err)
	}
	serveEcho(t, ln)

	ourParams := awg2
	ourParams.I[0] = "<b 0xc000000001><r 64><t>"
	ourCfg := awgconf.Config{
		Interface: awgconf.Interface{PrivateKey: ourKey, Params: ourParams},
		Peers: []awgconf.Peer{{
			PublicKey:  providerKey.Public(),
			Endpoint:   endpointOf(t, provider.IpcGet),
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
		}},
	}
	ours, ournet := newDevice(t, "lotsman-upstream", ourAddr)
	if err := ours.Configure(ourCfg.UAPI()); err != nil {
		t.Fatal(err)
	}
	if err := ours.Up(); err != nil {
		t.Fatal(err)
	}

	c, err := ournet.DialTCPAddrPort(netip.AddrPortFrom(providerAddr, 80))
	if err != nil {
		t.Fatalf("dial through AWG 2.0 provider: %v", err)
	}
	checkEcho(t, c)
}

// awg3 turns on everything AWG 3.1 adds. Header protection needs S1-S4 >= 12.
func awg3(key awgconf.Key) awgconf.Params {
	p := awg2
	p.S3, p.S4 = 12, 12
	p.HeaderProtectionKey = key
	p.ContentPaddingAddition = awgconf.Range{Lo: 0, Hi: 64}
	p.RandomTrailers = true
	p.DisableCookies = true
	return p
}

// awg3Pair starts a server and a client device with the given parameters and
// an echo server behind the server. dial connects to it through the tunnel.
func awg3Pair(t *testing.T, srvParams, cliParams awgconf.Params) (srv, cli *Device, dial func(context.Context) (net.Conn, error)) {
	t.Helper()
	srvKey, _ := awgconf.GeneratePrivateKey()
	cliKey, _ := awgconf.GeneratePrivateKey()
	srvAddr, cliAddr := netip.MustParseAddr("10.202.0.1"), netip.MustParseAddr("10.202.0.2")

	srv, srvNet := newDevice(t, "server", srvAddr)
	srvCfg := awgconf.Config{
		Interface: awgconf.Interface{PrivateKey: srvKey, Params: srvParams},
		Peers:     []awgconf.Peer{{PublicKey: cliKey.Public(), AllowedIPs: []netip.Prefix{netip.PrefixFrom(cliAddr, 32)}}},
	}
	if err := srv.Configure(srvCfg.UAPI()); err != nil {
		t.Fatal(err)
	}
	if err := srv.Up(); err != nil {
		t.Fatal(err)
	}
	ln, err := srvNet.ListenTCPAddrPort(netip.AddrPortFrom(srvAddr, 80))
	if err != nil {
		t.Fatal(err)
	}
	serveEcho(t, ln)

	cli, cliNet := newDevice(t, "client", cliAddr)
	cliCfg := awgconf.Config{
		Interface: awgconf.Interface{PrivateKey: cliKey, Params: cliParams},
		Peers: []awgconf.Peer{{
			PublicKey:  srvKey.Public(),
			Endpoint:   endpointOf(t, srv.dev.IpcGet),
			AllowedIPs: []netip.Prefix{netip.PrefixFrom(srvAddr, 32)},
		}},
	}
	if err := cli.Configure(cliCfg.UAPI()); err != nil {
		t.Fatal(err)
	}
	if err := cli.Up(); err != nil {
		t.Fatal(err)
	}
	dial = func(ctx context.Context) (net.Conn, error) {
		return cliNet.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(srvAddr, 80))
	}
	return srv, cli, dial
}

// Two 3.1 devices with header protection, random trailers and content
// padding complete a handshake and carry data.
func TestHeaderProtectionAndTrailers(t *testing.T) {
	hpk, _ := awgconf.GeneratePrivateKey()
	params := awg3(hpk)
	_, cli, dial := awg3Pair(t, params, params)

	// The runtime applied every AWG 3.x key that awgconf rendered.
	raw, err := cli.dev.IpcGet()
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"header_protection_key":    hpk.Hex(),
		"content_padding_addition": "0-64",
		"random_trailers":          "1",
		"disable_cookies":          "1",
	} {
		if got := uapiValue(raw, key); got != want {
			t.Errorf("IpcGet %s = %q, want %q", key, got, want)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dial(ctx)
	if err != nil {
		t.Fatalf("dial with header protection and trailers: %v", err)
	}
	checkEcho(t, c)
}

// A HeaderProtectionKey that differs from the server's blocks the handshake;
// fixing only the key lets it complete.
func TestHeaderProtectionKeyMismatch(t *testing.T) {
	srvKey, _ := awgconf.GeneratePrivateKey()
	cliKey, _ := awgconf.GeneratePrivateKey()
	cliParams := awg3(cliKey)
	cliParams.RekeyTimeout = awgconf.Range{Lo: 1, Hi: 1} // retry the handshake every second
	_, cli, dial := awg3Pair(t, awg3(srvKey), cliParams)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if c, err := dial(ctx); err == nil {
		c.Close()
		t.Fatal("dial succeeded with mismatched HeaderProtectionKey")
	}
	if peers, err := cli.Peers(); err != nil || len(peers) != 1 || !peers[0].LastHandshake.IsZero() {
		t.Fatalf("handshake with mismatched key: %+v, %v", peers, err)
	}

	if err := cli.Configure("header_protection_key=" + srvKey.Hex() + "\n"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dial(ctx)
	if err != nil {
		t.Fatalf("dial after fixing HeaderProtectionKey: %v", err)
	}
	checkEcho(t, c)
}
