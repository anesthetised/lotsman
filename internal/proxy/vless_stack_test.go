//go:build !race

// The tests here run Xray's VLESS Vision server, which races (see
// vlesstest), so they are left out of race-detector builds.

package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/proxy/vlesstest"
)

// newVLESSServer starts a Vision server whose echo, hello and DNS targets
// are the test targets, redirected to loopback.
func newVLESSServer(t *testing.T) *vlesstest.Server {
	t.Helper()
	return vlesstest.NewServer(t, vlessTargets(t), nil)
}

// Two VLESS upstreams in one process carry TCP and UDP independently.
func TestVLESSStacksCarryTCPAndUDP(t *testing.T) {
	sa, sb := newVLESSServer(t), newVLESSServer(t)
	vlesstest.WaitReady(t, sa, sb)
	a := newVLESSFlow(t, sa, sa.Link())
	b := newVLESSFlow(t, sb, sb.LinkWith(url.Values{"flow": {flowVisionUDP443}, "fp": {"firefox"}}))
	for i, f := range []*vlessFlow{a, b} {
		f.echo(t, "hello")
		f.echo(t, strings.Repeat("bulk ", 200_000)) // 1 MB, many segments
		f.hello(t)
		f.dns(t, uint16(i))
	}
	if st := a.stack.Stats(); st.Refused != 0 {
		t.Errorf("stats = %+v", st)
	}

	// Closing one stack, and its Xray instance, leaves the other working.
	if err := a.stack.Close(); err != nil {
		t.Fatal(err)
	}
	b.echo(t, "after a closed")
	b.dns(t, 9)
}

// New dialers, as after the server moved or its link changed, take new
// flows; closing the old Xray instance leaves its open flows working.
func TestVLESSSetDialers(t *testing.T) {
	a, b := newVLESSServer(t), newVLESSServer(t)
	vlesstest.WaitReady(t, a, b)
	f := newVLESSFlow(t, a, a.Link())
	held := f.dial(t)
	go io.WriteString(held, "old")
	if _, err := io.ReadFull(held, make([]byte, 3)); err != nil {
		t.Fatal(err)
	}
	key, err := ParseSpecKey(b.Link())
	if err != nil {
		t.Fatal(err)
	}
	d, err := key.Dialers(b.Addr())
	if err != nil {
		t.Fatal(err)
	}
	f.stack.SetDialers(d)
	a.Close() // no new connections to a: new flows must use b
	f.echo(t, "via b")
	go io.WriteString(held, "still")
	if _, err := io.ReadFull(held, make([]byte, 5)); err != nil {
		t.Fatalf("open flow after the switch: %v", err)
	}
}

// A VLESS dial returns before the server is reached, so the stack accepts
// the client's handshake; a server that is down must then reset the flow
// quickly rather than leave it hanging.
func TestVLESSServerDownResetsClient(t *testing.T) {
	s := newVLESSServer(t)
	vlesstest.WaitReady(t, s)
	f := newVLESSFlow(t, s, s.Link())
	f.echo(t, "up")
	s.Close()
	c := f.dial(t)
	start := time.Now()
	io.WriteString(c, "x")
	_, err := c.Read(make([]byte, 1))
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("read with the server down: %v, want a reset", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("reset took %v", d)
	}
}

// A client with the wrong server key never gets data through: the server
// treats it as a stranger and relays it to the camouflage site.
func TestVLESSWrongKey(t *testing.T) {
	s := newVLESSServer(t)
	vlesstest.WaitReady(t, s)
	f := newVLESSFlow(t, s, s.LinkWith(url.Values{"pbk": {"wsxdeEB2O7Mqa1ny8bxMHOMyTzvWvvVQXqhNBNPMn2E"}}))
	c := f.dial(t)
	io.WriteString(c, "hello")
	if _, err := io.ReadFull(c, make([]byte, 5)); err == nil {
		t.Fatal("echo succeeded with a wrong server key")
	}
}

// Xray's log reaches slog.
func TestVLESSLogsXray(t *testing.T) {
	s := newVLESSServer(t)
	vlesstest.WaitReady(t, s)
	var logs syncLog
	LogXray(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { LogXray(slog.New(slog.DiscardHandler)) })
	f := newVLESSFlow(t, s, s.Link())
	s.Close()
	for range 2 {
		c := f.dial(t)
		io.WriteString(c, "x")
		c.Read(make([]byte, 1))
		c.Close()
	}
	waitFor(t, "two failed flows logged", func() bool {
		return strings.Count(logs.String(), "failed to find an available destination") >= 2
	})
	// Xray reports a failed flow as info: a debug message here.
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "failed to find an available destination") &&
			(!strings.Contains(line, "level=DEBUG") || !strings.Contains(line, "component=xray")) {
			t.Errorf("failed flow logged as %s", line)
		}
	}
	if strings.Contains(logs.String(), s.ID()) {
		t.Error("the log shows the user ID")
	}
}
