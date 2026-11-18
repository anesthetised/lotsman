package tunnel

import (
	"net/netip"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/awgconf"
)

// The runtime reads S4 before it blocks in the TUN read, so a device configured
// after creation must still pad the very first packet it sends. One UDP
// datagram, no retransmit: if it goes out unpadded the peer drops it.
func TestFirstPacketCarriesTransportPadding(t *testing.T) {
	srvKey, _ := awgconf.GeneratePrivateKey()
	cliKey, _ := awgconf.GeneratePrivateKey()
	srvAddr, cliAddr := netip.MustParseAddr("10.203.0.1"), netip.MustParseAddr("10.203.0.2")
	if awg2.S4 == 0 {
		t.Fatal("test needs S4 > 0")
	}

	srv, srvNet := newDevice(t, "server", srvAddr)
	srvCfg := awgconf.Config{
		Interface: awgconf.Interface{PrivateKey: srvKey, Params: awg2},
		Peers:     []awgconf.Peer{{PublicKey: cliKey.Public(), AllowedIPs: []netip.Prefix{netip.PrefixFrom(cliAddr, 32)}}},
	}
	if err := srv.Configure(srvCfg.UAPI()); err != nil {
		t.Fatal(err)
	}
	if err := srv.Up(); err != nil {
		t.Fatal(err)
	}
	pc, err := srvNet.ListenUDPAddrPort(netip.AddrPortFrom(srvAddr, 9000))
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	cli, cliNet := newDevice(t, "client", cliAddr)
	// Let the runtime's reader goroutine reach its blocking TUN read first; this
	// is the order a real deployment sees, and with a runtime that does not
	// re-check S4 at encryption it makes the loss deterministic instead of
	// depending on goroutine scheduling.
	time.Sleep(100 * time.Millisecond)
	cliCfg := awgconf.Config{
		Interface: awgconf.Interface{PrivateKey: cliKey, Params: awg2},
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

	c, err := cliNet.DialUDPAddrPort(netip.AddrPort{}, netip.AddrPortFrom(srvAddr, 9000))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}

	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("first packet did not arrive (sent without S4 padding?): %v", err)
	}
	if string(buf[:n]) != "first" {
		t.Fatalf("got %q", buf[:n])
	}
}
