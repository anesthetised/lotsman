//go:build linux && integration

package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/proto"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/control"
)

// TestControlSocket runs the daemon over a socket a crashed daemon left
// behind: serve replaces it with one only root and the lotsman-web group
// may use, streams status over it, and removes it on stop.
func TestControlSocket(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates TUNs and programs the kernel")
	}
	group, err := user.LookupGroup(control.Group)
	if err != nil {
		t.Skipf("needs the %s group, which deploy/Dockerfile.test creates: %v", control.Group, err)
	}
	os.MkdirAll(filepath.Dir(control.SocketPath), 0o755)
	stale, err := net.Listen("unix", control.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()

	dir := t.TempDir()
	nl := newProvider(t, "nl", netip.MustParsePrefix("10.100.0.0/24"), 51001)
	os.WriteFile(filepath.Join(dir, "nl.conf"), []byte(nl.conf), 0o600)
	cfgPath := filepath.Join(dir, "lotsman.yaml")
	os.WriteFile(cfgPath, []byte(`
listen: 127.0.0.1:51820
endpoint: 127.0.0.1:51820
state_dir: `+filepath.Join(dir, "state")+`
upstreams:
  - {name: nl, conf: `+filepath.Join(dir, "nl.conf")+`, geo: NL, provider: acme}
health: {interval: 500ms, probe: 198.51.100.1:443, down_after: 2, up_after: 1}
`), 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, cfgPath, slog.LevelInfo) }()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			if err := <-served; err != nil {
				t.Errorf("serve: %v", err)
			}
		}
	}
	t.Cleanup(stop)
	eventually(t, "daemon status", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "state", "status.json"))
		return err == nil
	})

	fi, err := os.Stat(control.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if gid := strconv.Itoa(int(fi.Sys().(*syscall.Stat_t).Gid)); fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o660 || gid != group.Gid {
		t.Errorf("socket mode %v gid %s, want socket 0660 gid %s", fi.Mode(), gid, group.Gid)
	}

	if _, err := capture(t, "-config", cfgPath, "device", "add", "alice/phone", "NL"); err != nil {
		t.Fatal(err)
	}
	stream, err := controlClient(control.SocketPath).Watch(t.Context(), &lotsmanv1.WatchStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// alice is the first peer in the store; routing her may take a round.
	want := &lotsmanv1.PeerStatus{PeerId: "p_1", User: "alice", Device: "phone", Address: "10.77.0.2", Upstream: "nl",
		Traffic: &lotsmanv1.Traffic{}}
	var got *lotsmanv1.StatusSnapshot
	deadline := time.Now().Add(15 * time.Second)
	for n := 0; ; n++ {
		if got, err = stream.Receive(); err != nil {
			t.Fatalf("snapshot %d: %v", n, err)
		}
		// She never connects, so her peer has no handshake and no traffic
		// once the sampler has read it.
		if n >= 1 && len(got.Peers) == 1 && proto.Equal(got.Peers[0], want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no snapshot with %v after %d: last %v", want, n+1, got)
		}
	}
	if u := got.Upstreams; len(u) != 1 || u[0].Name != "nl" || u[0].Kind != lotsmanv1.UpstreamKind_UPSTREAM_KIND_AWG ||
		u[0].Geo != "NL" || u[0].Provider != "acme" || u[0].State != lotsmanv1.UpstreamState_UPSTREAM_STATE_UP || u[0].Routed != 1 {
		t.Errorf("upstreams = %v", u)
	}
	if got.ClientMtu == 0 {
		t.Error("client_mtu unset")
	}

	// The daemon's shutdown ends the stream and removes the socket.
	stop()
	if _, err := stream.Receive(); err == nil {
		t.Error("stream still open after the daemon stopped")
	}
	if _, err := os.Lstat(control.SocketPath); !os.IsNotExist(err) {
		t.Errorf("socket left behind: %v", err)
	}
}

// controlClient is a status client over the unix socket at path.
func controlClient(path string) lotsmanv1connect.StatusServiceClient {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	return lotsmanv1connect.NewStatusServiceClient(connect.NewClient(connecthttp.NewTransport(hc, "http://lotsman")))
}
