//go:build linux && integration

package tunnel

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/conn"
	"github.com/amnezia-vpn/amneziawg-go/tun/netstack"

	"github.com/anesthetised/lotsman/internal/awgconf"
)

// Run only in the disposable Linux integration container: these addresses and
// the dummy interface belong to its network namespace.
func bindTestAddresses(t *testing.T) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %v: %s", args, err, out)
		}
	}
	run("link", "add", "lm-bind13", "type", "dummy")
	t.Cleanup(func() { exec.Command("ip", "link", "del", "lm-bind13").Run() })
	run("link", "set", "lm-bind13", "up")
	for _, addr := range []string{"192.0.2.10/24", "192.0.2.11/24"} {
		run("addr", "add", addr, "dev", "lm-bind13")
	}
	for _, addr := range []string{"2001:db8:13::10/64", "2001:db8:13::11/64", "fe80::13/64"} {
		run("-6", "addr", "add", addr, "dev", "lm-bind13", "nodad")
	}
}

func assertBoundSocket(t *testing.T, addr netip.Addr, port uint16) {
	t.Helper()
	addr = addr.Unmap().WithZone("")
	bytes := addr.AsSlice()
	// /proc prints each native-endian 32-bit word of the socket address.
	for i := 0; i < len(bytes); i += 4 {
		bytes[i], bytes[i+1], bytes[i+2], bytes[i+3] = bytes[i+3], bytes[i+2], bytes[i+1], bytes[i]
	}
	family := "udp6"
	if addr.Is4() {
		family = "udp"
	}
	want := family + " " + strings.ToUpper(hex.EncodeToString(bytes)) + fmt.Sprintf(":%04X", port)
	var found []string
	for _, name := range []string{"udp", "udp6"} {
		data, err := os.ReadFile("/proc/net/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 1 && strings.HasSuffix(fields[1], fmt.Sprintf(":%04X", port)) {
				found = append(found, name+" "+fields[1])
			}
		}
	}
	if len(found) != 1 || found[0] != want {
		t.Fatalf("bound sockets = %v, want only %s", found, want)
	}
}

type bindPacket struct {
	data string
	err  error
}

func receiveBindPacket(fn conn.ReceiveFunc) <-chan bindPacket {
	result := make(chan bindPacket, 1)
	go func() {
		buf := make([]byte, 2048)
		sizes := make([]int, 1)
		_, err := fn([][]byte{buf}, sizes, make([]conn.Endpoint, 1))
		result <- bindPacket{string(buf[:sizes[0]]), err}
	}()
	return result
}

func sendBindPacket(t *testing.T, addr string, port uint16, marker string) {
	t.Helper()
	dst := netip.AddrPortFrom(netip.MustParseAddr(addr), port)
	socket, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(dst))
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if _, err := socket.Write([]byte(marker)); err != nil {
		t.Fatal(err)
	}
}

func TestAddressBindExposure(t *testing.T) {
	bindTestAddresses(t)
	for _, tc := range []struct {
		name, address string
		allowed       []string
		excluded      string
	}{
		{"IPv4 loopback", "127.0.0.1", []string{"127.0.0.1"}, "192.0.2.10"},
		{"IPv4 assigned", "192.0.2.10", []string{"192.0.2.10"}, "192.0.2.11"},
		{"IPv4 wildcard", "0.0.0.0", []string{"127.0.0.1", "192.0.2.10"}, "::1"},
		{"IPv6 loopback", "::1", []string{"::1"}, "2001:db8:13::10"},
		{"IPv6 assigned", "2001:db8:13::10", []string{"2001:db8:13::10"}, "2001:db8:13::11"},
		{"IPv6 wildcard", "::", []string{"::1", "2001:db8:13::10"}, "127.0.0.1"},
		{"mapped IPv4", "::ffff:127.0.0.1", []string{"127.0.0.1"}, "192.0.2.10"},
		{"scoped IPv6", "fe80::13%lm-bind13", []string{"fe80::13%lm-bind13"}, "2001:db8:13::10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := netip.MustParseAddr(tc.address)
			bind := newAddressBind(addr)
			defer bind.Close()
			fns, port, err := bind.Open(0)
			if err != nil {
				t.Fatal(err)
			}
			if port == 0 || len(fns) != 1 {
				t.Fatalf("Open = %d receivers, port %d", len(fns), port)
			}
			assertBoundSocket(t, addr, port)
			for _, allowed := range tc.allowed {
				result := receiveBindPacket(fns[0])
				sendBindPacket(t, allowed, port, "allowed:"+allowed)
				select {
				case got := <-result:
					if got.err != nil || got.data != "allowed:"+allowed {
						t.Fatalf("receive = %+v", got)
					}
				case <-time.After(time.Second):
					t.Fatal("selected address did not receive packet")
				}
			}
			if tc.excluded != "" {
				result := receiveBindPacket(fns[0])
				sendBindPacket(t, tc.excluded, port, "excluded")
				select {
				case got := <-result:
					t.Fatalf("excluded address delivered packet: %+v", got)
				case <-time.After(100 * time.Millisecond):
				}
				bind.Close()
				select {
				case got := <-result:
					if !errors.Is(got.err, net.ErrClosed) {
						t.Fatalf("closed receive = %v", got.err)
					}
				case <-time.After(time.Second):
					t.Fatal("Close did not unblock receiver")
				}
			}
		})
	}
}

func TestBoundDeviceSocket(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "::1"} {
		t.Run(address, func(t *testing.T) {
			tn, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.77.0.1")}, nil, 1400)
			if err != nil {
				t.Fatal(err)
			}
			addr := netip.MustParseAddr(address)
			d := NewBound("bound", tn, addr, slog.New(slog.NewTextHandler(io.Discard, nil)))
			defer d.Close()
			key, err := awgconf.GeneratePrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			if err := d.Configure((&awgconf.Config{Interface: awgconf.Interface{PrivateKey: key}}).UAPI()); err != nil {
				t.Fatal(err)
			}
			if err := d.Up(); err != nil {
				t.Fatal(err)
			}
			raw, err := d.dev.IpcGet()
			if err != nil {
				t.Fatal(err)
			}
			var port uint64
			for _, line := range strings.Split(raw, "\n") {
				if value, ok := strings.CutPrefix(line, "listen_port="); ok {
					port, err = strconv.ParseUint(value, 10, 16)
				}
			}
			if err != nil || port == 0 {
				t.Fatalf("device port = %d: %v", port, err)
			}
			assertBoundSocket(t, addr, uint16(port))
		})
	}
}

func TestAddressBindLifecycleAndSend(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "::1"} {
		t.Run(addr, func(t *testing.T) {
			bind := newAddressBind(netip.MustParseAddr(addr))
			defer bind.Close()
			fns, port, err := bind.Open(0)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := bind.Open(port); !errors.Is(err, conn.ErrBindAlreadyOpen) {
				t.Fatalf("second Open = %v", err)
			}
			pending := receiveBindPacket(fns[0])
			if err := bind.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-pending:
				if !errors.Is(got.err, net.ErrClosed) {
					t.Fatalf("closed receiver = %v", got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close left receiver blocked")
			}
			if _, _, err := bind.Open(port); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-receiveBindPacket(fns[0]):
				if !errors.Is(got.err, net.ErrClosed) {
					t.Fatalf("old receiver after reopen = %v", got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("old receiver attached to reopened socket")
			}
			destination, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr(addr), 0)))
			if err != nil {
				t.Fatal(err)
			}
			defer destination.Close()
			ep, err := bind.ParseEndpoint(destination.LocalAddr().String())
			if err != nil {
				t.Fatal(err)
			}
			if bind.BatchSize() != 1 || bind.SetMark(0) != nil || bind.SetMark(1) == nil {
				t.Fatal("unexpected batch size or mark support")
			}
			if err := bind.Send([][]byte{[]byte("one"), []byte("two"), []byte("three")}, ep); err != nil {
				t.Fatal(err)
			}
			destination.SetReadDeadline(time.Now().Add(time.Second))
			for _, want := range []string{"one", "two", "three"} {
				buf := make([]byte, 16)
				n, source, err := destination.ReadFromUDPAddrPort(buf)
				if err != nil || string(buf[:n]) != want || source.Port() != port || source.Addr().Unmap().String() != addr {
					t.Fatalf("send received %q from %s: %v", buf[:n], source, err)
				}
			}
			bind.Close()
			if err := bind.Send([][]byte{[]byte("closed")}, ep); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("send after Close = %v", err)
			}
		})
	}
}

func TestAddressBindOpenErrors(t *testing.T) {
	for _, tc := range []struct{ local, unassigned string }{{"127.0.0.1", "192.0.2.99"}, {"::1", "2001:db8:13::99"}} {
		t.Run(tc.local, func(t *testing.T) {
			occupied, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr(tc.local), 0)))
			if err != nil {
				t.Fatal(err)
			}
			defer occupied.Close()
			port := occupied.LocalAddr().(*net.UDPAddr).AddrPort().Port()
			bind := newAddressBind(netip.MustParseAddr(tc.local))
			defer bind.Close()
			if _, _, err := bind.Open(port); !errors.Is(err, syscall.EADDRINUSE) {
				t.Fatalf("occupied Open = %v", err)
			}
			assertBoundSocket(t, netip.MustParseAddr(tc.local), port)
			unassigned := newAddressBind(netip.MustParseAddr(tc.unassigned))
			defer unassigned.Close()
			if _, _, err := unassigned.Open(port); !errors.Is(err, syscall.EADDRNOTAVAIL) {
				t.Fatalf("unassigned Open on port %s = %v", strconv.Itoa(int(port)), err)
			}
		})
	}
}
