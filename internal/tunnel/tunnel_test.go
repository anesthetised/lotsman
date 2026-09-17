package tunnel

import (
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/tun/netstack"

	"github.com/anesthetised/lotsman/internal/awgconf"
)

var awg2 = awgconf.Params{
	Jc: 3, Jmin: 40, Jmax: 70,
	S1: 15, S2: 25, S3: 10, S4: 8,
	H1: awgconf.Range{Lo: 100, Hi: 200}, H2: awgconf.Range{Lo: 300, Hi: 400},
	H3: awgconf.Range{Lo: 500, Hi: 600}, H4: awgconf.Range{Lo: 700, Hi: 800},
}

func newDevice(t *testing.T, name string, addr netip.Addr) (*Device, *netstack.Net) {
	t.Helper()
	tn, tnet, err := netstack.CreateNetTUN([]netip.Addr{addr}, nil, 1400)
	if err != nil {
		t.Fatal(err)
	}
	d := New(name, tn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(d.Close)
	return d, tnet
}

func TestHandshakeAndTransport(t *testing.T) {
	srvKey, _ := awgconf.GeneratePrivateKey()
	cliKey, _ := awgconf.GeneratePrivateKey()
	srvAddr, cliAddr := netip.MustParseAddr("10.77.0.1"), netip.MustParseAddr("10.77.0.2")

	srv, srvNet := newDevice(t, "server", srvAddr)
	cli, cliNet := newDevice(t, "client", cliAddr)

	srvCfg := awgconf.Config{Interface: awgconf.Interface{PrivateKey: srvKey, ListenPort: 51999, Params: awg2}}
	if err := srv.Configure(srvCfg.UAPI()); err != nil {
		t.Fatal(err)
	}
	if err := srv.AddPeer(awgconf.Peer{PublicKey: cliKey.Public(), AllowedIPs: []netip.Prefix{netip.PrefixFrom(cliAddr, 32)}}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Up(); err != nil {
		t.Fatal(err)
	}

	cliParams := awg2
	cliParams.I[0] = "<b 0xc000000001><r 64><t>"
	cliCfg := awgconf.Config{
		Interface: awgconf.Interface{PrivateKey: cliKey, Params: cliParams},
		Peers: []awgconf.Peer{{
			PublicKey:  srvKey.Public(),
			Endpoint:   "127.0.0.1:51999",
			AllowedIPs: []netip.Prefix{netip.PrefixFrom(srvAddr, 32)},
		}},
	}
	if err := cli.Configure(cliCfg.UAPI()); err != nil {
		t.Fatal(err)
	}
	if err := cli.Up(); err != nil {
		t.Fatal(err)
	}

	ln, err := srvNet.ListenTCPAddrPort(netip.AddrPortFrom(srvAddr, 8080))
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

	c, err := cliNet.DialTCPAddrPort(netip.AddrPortFrom(srvAddr, 8080))
	if err != nil {
		t.Fatalf("dial through tunnel: %v", err)
	}
	payload := strings.Repeat("x", 3000)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("echo through tunnel: %v", err)
	}
	c.Close()

	peers, err := srv.Peers()
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].PublicKey != cliKey.Public() {
		t.Fatalf("server peers = %+v", peers)
	}
	p := peers[0]
	if p.LastHandshake.IsZero() || time.Since(p.LastHandshake) > time.Minute {
		t.Errorf("last handshake = %v", p.LastHandshake)
	}
	if p.RxBytes < uint64(len(payload)) || p.TxBytes < uint64(len(payload)) {
		t.Errorf("counters rx=%d tx=%d", p.RxBytes, p.TxBytes)
	}
	if !strings.HasPrefix(p.Endpoint, "127.0.0.1:") {
		t.Errorf("endpoint = %q", p.Endpoint)
	}

	if err := srv.RemovePeer(cliKey.Public()); err != nil {
		t.Fatal(err)
	}
	if peers, _ := srv.Peers(); len(peers) != 0 {
		t.Errorf("peer survived RemovePeer: %+v", peers)
	}
}

func TestParsePeers(t *testing.T) {
	k1, _ := awgconf.GeneratePrivateKey()
	k2, _ := awgconf.GeneratePrivateKey()
	raw := "listen_port=51820\ns4=8\n" +
		"public_key=" + k1.Hex() + "\nendpoint=1.2.3.4:5\nlast_handshake_time_sec=1700000000\nlast_handshake_time_nsec=5\nrx_bytes=10\ntx_bytes=20\nallowed_ip=10.0.0.2/32\n" +
		"public_key=" + k2.Hex() + "\nlast_handshake_time_sec=0\nlast_handshake_time_nsec=0\nrx_bytes=0\ntx_bytes=0\n" +
		"errno=0\n"
	peers, err := parsePeers(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d peers", len(peers))
	}
	if peers[0].PublicKey != k1 || peers[0].Endpoint != "1.2.3.4:5" || peers[0].RxBytes != 10 || peers[0].TxBytes != 20 ||
		!peers[0].LastHandshake.Equal(time.Unix(1700000000, 5)) {
		t.Errorf("peer 1 = %+v", peers[0])
	}
	if peers[1].PublicKey != k2 || !peers[1].LastHandshake.IsZero() {
		t.Errorf("peer 2 = %+v", peers[1])
	}
	if _, err := parsePeers("public_key=zz\n"); err == nil {
		t.Error("bad key accepted")
	}
}
