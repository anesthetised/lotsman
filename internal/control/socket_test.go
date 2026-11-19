package control

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/daemon"
)

// socketPath is a socket path short enough for sun_path (104 bytes on
// macOS), which t.TempDir there is not.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "run", "control.sock")
}

// ownGroup is a group the test may chown to without privileges.
func ownGroup(t *testing.T) (name string, gid int) {
	t.Helper()
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Skip("own group:", err)
	}
	return g.Name, os.Getgid()
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// client returns a status client over the unix socket at path.
func client(path string) lotsmanv1connect.StatusServiceClient {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	return lotsmanv1connect.NewStatusServiceClient(connect.NewClient(connecthttp.NewTransport(hc, "http://lotsman")))
}

func TestListenServesAndRemovesSocket(t *testing.T) {
	path := socketPath(t)
	group, gid := ownGroup(t)
	ln, err := Listen(path, group, discard())
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o660 || int(fi.Sys().(*syscall.Stat_t).Gid) != gid {
		t.Errorf("socket mode %v gid %d, want socket 0660 gid %d", fi.Mode(), fi.Sys().(*syscall.Stat_t).Gid, gid)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("socket directory holds %v, want the socket alone", entries)
	}

	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, ln, newFakeSource(daemon.Status{Upstreams: []daemon.UpstreamStatus{{Name: "nl"}}}))
	}()
	stream, err := client(path).Watch(t.Context(), &lotsmanv1.WatchStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := stream.Receive(); err != nil || len(s.Upstreams) != 1 {
		t.Fatalf("snapshot over the socket: %v, %v", s, err)
	}

	// The daemon stopping ends open streams, and the socket goes with it.
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve still running with a stream open")
	}
	if _, err := stream.Receive(); err == nil {
		t.Error("stream still open after the server stopped")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("socket left behind: %v", err)
	}
}

// Without the group, only root can connect, and the operator is told why.
func TestListenWithoutGroup(t *testing.T) {
	path := socketPath(t)
	var log bytes.Buffer
	ln, err := Listen(path, "lotsman-no-such-group", slog.New(slog.NewTextHandler(&log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v, want 0600", fi.Mode())
	}
	if !strings.Contains(log.String(), "root-only") || !strings.Contains(log.String(), "lotsman-no-such-group") {
		t.Errorf("no warning naming the group: %q", log.String())
	}
}

// A daemon that crashed leaves its socket behind; the next one replaces it.
func TestListenReplacesStaleSocket(t *testing.T) {
	path := socketPath(t)
	group, _ := ownGroup(t)
	os.MkdirAll(filepath.Dir(path), 0o755)
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket not left behind: %v", err)
	}

	ln, err := Listen(path, group, discard())
	if err != nil {
		t.Fatalf("stale socket blocks start: %v", err)
	}
	defer ln.Close()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("new socket: %v", err)
	}
	c.Close()
}

// A socket that still answers belongs to a running daemon (in another
// network namespace, past the instance locks): it is neither taken over nor
// removed.
func TestListenRefusesLiveSocket(t *testing.T) {
	path := socketPath(t)
	group, _ := ownGroup(t)
	live, err := Listen(path, group, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := Listen(path, group, discard()); err == nil || !strings.Contains(err.Error(), "another process") {
		t.Errorf("second listen: %v", err)
	}
	accepted := make(chan error, 1)
	go func() {
		c, err := live.Accept()
		if err == nil {
			c.Close()
		}
		accepted <- err
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("live socket gone after a refused listen: %v", err)
	}
	c.Close()
	if err := <-accepted; err != nil {
		t.Errorf("live socket did not accept: %v", err)
	}
}
