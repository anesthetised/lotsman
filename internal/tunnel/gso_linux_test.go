//go:build linux && integration

package tunnel

import (
	"bytes"
	"net"
	"net/netip"
	"os/exec"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
)

func TestUpstreamWithoutUDPGSOAtSmallPMTU(t *testing.T) {
	// Isolated integration container. This local /32 leaves ordinary
	// loopback traffic and the container's other routes alone.
	address := netip.MustParseAddr("127.0.0.165")
	route := []string{"local", address.String() + "/32", "dev", "lo", "table", "local"}
	run := func(action string, args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", append([]string{"-4", "route", action}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ip route %s %v: %v: %s", action, args, err, out)
		}
	}
	run("add", append(route, "mtu", "556")...)
	t.Cleanup(func() { run("del", route...) })

	receiver, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(address, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	bind := newUpstreamBind(true)
	receive, port, err := bind.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	defer bind.Close()
	if _, err := receiver.WriteToUDPAddrPort([]byte{1}, netip.AddrPortFrom(address, port)); err != nil {
		t.Fatal(err)
	}
	incoming := make(chan conn.Endpoint, 1)
	go func() {
		bufs := make([][]byte, bind.BatchSize())
		for i := range bufs {
			bufs[i] = make([]byte, 2048)
		}
		sizes := make([]int, len(bufs))
		endpoints := make([]conn.Endpoint, len(bufs))
		n, err := receive[0](bufs, sizes, endpoints)
		if err != nil || n != 1 || sizes[0] != 1 {
			incoming <- nil
			return
		}
		incoming <- endpoints[0]
	}()
	var endpoint conn.Endpoint
	select {
	case endpoint = <-incoming:
		if endpoint == nil || endpoint.SrcIP().IsValid() {
			t.Fatalf("receive gave endpoint %v, want a destination without a sticky source", endpoint)
		}
	case <-time.After(time.Second):
		t.Fatal("bind did not receive the test packet")
	}

	// The receiver learned our original local address. A later route change
	// must select the new source, even when replies use that learned endpoint.
	source := netip.MustParseAddr("127.0.0.1")
	run("replace", append(route, "mtu", "556", "src", source.String())...)

	for _, sizes := range [][]int{{528, 528}, {529, 529}, {1332, 1332}, {529, 0, 1332}} {
		var batch [][]byte
		for i, size := range sizes {
			packet := make([]byte, size, size*2)
			for j := range packet {
				packet[j] = byte(i + 1)
			}
			batch = append(batch, packet)
		}
		if err := receiver.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := bind.Send(batch, endpoint); err != nil {
			t.Fatalf("Send %v at PMTU 556: %v", sizes, err)
		}
		buf := make([]byte, 2048)
		for i, want := range batch {
			n, from, err := receiver.ReadFromUDPAddrPort(buf)
			if err != nil || !bytes.Equal(buf[:n], want) {
				t.Fatalf("batch %v packet %d: got %d bytes, want %d, error %v", sizes, i, n, len(want), err)
			}
			if from.Addr() != source {
				t.Fatalf("packet came from %s, want new route source %s", from, source)
			}
		}
	}
	if err := receiver.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if n, _, err := receiver.ReadFromUDP(make([]byte, 2048)); err == nil {
		t.Fatalf("received an extra %d-byte datagram", n)
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal(err)
	}
}
