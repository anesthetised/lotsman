package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/types/known/durationpb"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/control"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/traffic"
)

// fakeSource is a daemon with one upstream and one profile, which peers
// use. The test never calls the connection and history services.
type fakeSource struct {
	control.ConnectionSource
	control.HistorySource
}

func (fakeSource) Ready() <-chan struct{} {
	ready := make(chan struct{})
	close(ready)
	return ready
}
func (fakeSource) ClientMTU() int { return 1280 }
func (fakeSource) Status() daemon.Status {
	return daemon.Status{Upstreams: []daemon.UpstreamStatus{{Name: "nl", Kind: "awg"}}}
}
func (fakeSource) Traffic() traffic.Snapshot { return traffic.Snapshot{} }
func (fakeSource) Profiles() ([]config.Profile, []string, error) {
	return []config.Profile{{Name: "eu"}}, nil, nil
}
func (fakeSource) PutProfile(config.Profile) ([]string, error) { return nil, nil }
func (fakeSource) DeleteProfile(string) error                  { return store.ErrInUse }

// daemonSocket serves the real control API, over a fake daemon, on a unix
// socket, as lotsman serve does.
func daemonSocket(t *testing.T) string {
	t.Helper()
	// t.TempDir is too long for sun_path on macOS.
	dir, err := os.MkdirTemp("/tmp", "lw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- control.Serve(ctx, ln, fakeSource{}) }()
	t.Cleanup(func() { stop(); <-served })
	return path
}

// withOrigin is a browser on the panel's page.
type withOrigin struct{ origin string }

func (o withOrigin) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Origin", o.origin)
	return http.DefaultTransport.RoundTrip(r)
}

// browserPanel runs the panel on a real listener in front of the daemon
// socket and returns a Connect transport as the frontend's
// createConnectTransport({baseUrl: "/api"}) would use it. A nil auth is
// production's, over an empty web.db.
func browserPanel(t *testing.T, socket string, auth Authenticator) connect.Transport {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	origin := "http://localhost:" + port
	cfg, err := ParseConfig([]byte("public_url: " + origin + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if auth == nil {
		auth = newTestAuth(t, cfg)
	}
	srv.Config.Handler = New(Deps{Config: cfg, Auth: auth, Daemon: DaemonTransport(socket), UI: fstest.MapFS{}, Log: discardLog()})
	srv.Start()
	t.Cleanup(srv.Close)
	return connecthttp.NewTransport(&http.Client{Transport: withOrigin{origin}}, srv.URL+"/api")
}

// A Watch stream crosses the panel message by message: the first snapshot
// arrives at once, not when the stream ends or a buffer fills.
func TestForwardWatchStream(t *testing.T) {
	transport := browserPanel(t, daemonSocket(t), fakeAuth{operator: true})
	client := lotsmanv1connect.NewStatusServiceClient(connect.NewClient(transport))
	// A buffered stream would deliver nothing before this ends it.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	stream, err := client.Watch(ctx, &lotsmanv1.WatchStatusRequest{Interval: durationpb.New(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > control.MinInterval/2 {
		t.Errorf("first snapshot after %s: the panel buffers the stream", took)
	}
	if len(first.Upstreams) != 1 || first.Upstreams[0].Name != "nl" || first.ClientMtu != 1280 {
		t.Errorf("first snapshot = %v", first)
	}
	second, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if gap := second.Time.AsTime().Sub(first.Time.AsTime()); gap < control.MinInterval-50*time.Millisecond {
		t.Errorf("snapshots %s apart", gap)
	}
	cancel()
	if _, err := stream.Receive(); err == nil {
		t.Error("stream still open after the browser went away")
	}
}

// Unary calls and the daemon's own errors pass through as they are.
func TestForwardUnary(t *testing.T) {
	transport := browserPanel(t, daemonSocket(t), fakeAuth{operator: true, user: "alice"})
	profiles := lotsmanv1connect.NewProfileServiceClient(connect.NewClient(transport))
	if res, err := profiles.List(t.Context(), &lotsmanv1.ListProfilesRequest{}); err != nil || len(res.Profiles) != 1 || res.Profiles[0].Name != "eu" {
		t.Errorf("profile list: %v, %v", res, err)
	}
	if _, err := profiles.Delete(t.Context(), &lotsmanv1.DeleteProfileRequest{Name: "eu"}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("profile delete: %v, want the daemon's failed_precondition", err)
	}
	self := lotsmanv1connect.NewUserSelfServiceClient(connect.NewClient(transport))
	if _, err := self.ResolveLink(t.Context(), &lotsmanv1.ResolveLinkRequest{Token: "x"}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("resolve link: %v, want permission_denied from the panel", err)
	}
}

// Without a session, operator and user calls fail before the daemon, with
// a code the frontend can act on.
func TestForwardWithoutAuth(t *testing.T) {
	transport := browserPanel(t, daemonSocket(t), nil)
	client := lotsmanv1connect.NewStatusServiceClient(connect.NewClient(transport))
	stream, err := client.Watch(t.Context(), &lotsmanv1.WatchStatusRequest{})
	if err == nil {
		_, err = stream.Receive()
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("watch: %v, want unauthenticated", err)
	}
	self := lotsmanv1connect.NewUserSelfServiceClient(connect.NewClient(transport))
	if _, err := self.Get(t.Context(), &lotsmanv1.GetSelfRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("get self: %v, want unauthenticated", err)
	}
}

func TestForwardDaemonDown(t *testing.T) {
	transport := browserPanel(t, filepath.Join(t.TempDir(), "none.sock"), fakeAuth{operator: true})
	profiles := lotsmanv1connect.NewProfileServiceClient(connect.NewClient(transport))
	if _, err := profiles.List(t.Context(), &lotsmanv1.ListProfilesRequest{}); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("list: %v, want unavailable", err)
	}
}
