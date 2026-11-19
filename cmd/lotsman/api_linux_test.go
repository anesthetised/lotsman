//go:build linux && integration

package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/proto"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/control"
	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
)

// apiClient speaks the control API over the unix socket at path.
func apiClient(path string) *connect.Client {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	return connect.NewClient(connecthttp.NewTransport(hc, "http://lotsman"))
}

// TestControlProfilesAndConnections drives the profile and connection
// services through the daemon's socket: a profile made over the API serves
// a client, cannot be deleted while it does, and the client's download shows
// up as a live connection.
func TestControlProfilesAndConnections(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates TUNs and programs the kernel")
	}
	dir := t.TempDir()
	nl := newProvider(t, "nl", netip.MustParsePrefix("10.100.0.0/24"), 51001)
	os.WriteFile(filepath.Join(dir, "nl.conf"), []byte(nl.conf), 0o600)
	bulk, err := nl.tnet.ListenTCPAddrPort(netip.AddrPortFrom(internet, bulkPort))
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, bulk, streamBulk)
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
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	eventually(t, "daemon status", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "state", "status.json"))
		return err == nil
	})
	api := apiClient(control.SocketPath)
	profiles := lotsmanv1connect.NewProfileServiceClient(api)

	// The first tier takes the only upstream: the second never applies, with a warning.
	eu := &lotsmanv1.Profile{Name: "eu", Prefer: []*lotsmanv1.Matcher{{Geo: "NL"}, {Provider: "acme"}}}
	put, err := profiles.Put(t.Context(), &lotsmanv1.PutProfileRequest{Profile: eu})
	if err != nil {
		t.Fatal(err)
	}
	warned := func(warnings []string) bool {
		return slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, `profile "eu": tier 2`) })
	}
	if !warned(put.Warnings) {
		t.Errorf("put warnings = %q, want one for tier 2", put.Warnings)
	}
	if _, err := profiles.Put(t.Context(), &lotsmanv1.PutProfileRequest{Profile: &lotsmanv1.Profile{Name: "spare"}}); err != nil {
		t.Fatal(err)
	}
	// A tier no upstream matches is refused, even beside one that matches.
	if _, err := profiles.Put(t.Context(), &lotsmanv1.PutProfileRequest{Profile: &lotsmanv1.Profile{
		Name: "de", Prefer: []*lotsmanv1.Matcher{{Geo: "NL"}, {Geo: "DE"}},
	}}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("put de: %v, want invalid_argument", err)
	}
	// A profile no upstream can serve is refused.
	if _, err := profiles.Put(t.Context(), &lotsmanv1.PutProfileRequest{Profile: &lotsmanv1.Profile{
		Name: "us", Prefer: []*lotsmanv1.Matcher{{Geo: "US"}},
	}}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("put us: %v, want invalid_argument", err)
	}
	list, err := profiles.List(t.Context(), &lotsmanv1.ListProfilesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Profiles) != 2 || !proto.Equal(list.Profiles[0], eu) || list.Profiles[1].Name != "spare" || !warned(list.Warnings) {
		t.Errorf("list = %v", list)
	}
	if want := []*lotsmanv1.UpstreamLabels{{Name: "nl", Geo: "NL", Provider: "acme"}}; !slices.EqualFunc(list.Upstreams, want, func(a, b *lotsmanv1.UpstreamLabels) bool { return proto.Equal(a, b) }) {
		t.Errorf("list upstreams = %v, want %v", list.Upstreams, want)
	}
	if _, err := profiles.Delete(t.Context(), &lotsmanv1.DeleteProfileRequest{Name: "spare"}); err != nil {
		t.Errorf("delete spare: %v", err)
	}
	if _, err := profiles.Delete(t.Context(), &lotsmanv1.DeleteProfileRequest{Name: "spare"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("delete spare again: %v, want not_found", err)
	}

	out, err := capture(t, "-config", cfgPath, "user", "add", "alice", "-profile", "eu")
	if err != nil {
		t.Fatal(err)
	}
	alice := newClient(t, out)
	eventually(t, "alice exits via nl", 15*time.Second, func() bool {
		got, err := alice.exitVia(2 * time.Second)
		return err == nil && got == "nl"
	})
	if _, err := profiles.Delete(t.Context(), &lotsmanv1.DeleteProfileRequest{Name: "eu"}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("delete eu while alice uses it: %v, want failed_precondition", err)
	}

	connections := lotsmanv1connect.NewConnectionServiceClient(api)
	stream, err := connections.Watch(t.Context(), &lotsmanv1.WatchConnectionsRequest{PeerId: "p_404"})
	if err == nil {
		_, err = stream.Receive()
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("watch an unknown peer: %v, want not_found", err)
	}
	// alice is the first peer in the store.
	if stream, err = connections.Watch(t.Context(), &lotsmanv1.WatchConnectionsRequest{PeerId: "p_1"}); err != nil {
		t.Fatal(err)
	}
	alice.download(t)
	start := time.Now()
	destination := netip.AddrPortFrom(internet, bulkPort).String()
	for {
		got, err := stream.Receive()
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(got.Connections, func(c *lotsmanv1.Connection) bool { return c.Destination == destination })
		if i >= 0 {
			c := got.Connections[i]
			if c.PeerId != "p_1" || c.Protocol != lotsmanv1.Protocol_PROTOCOL_TCP || c.Upstream != "nl" ||
				!strings.HasPrefix(c.Source, "10.77.0.2:") {
				t.Fatalf("connection = %v", c)
			}
			// A download: more bytes towards the client than from it.
			if c.Traffic.GetDownBytes() > c.Traffic.GetUpBytes() && c.Traffic.GetUpBytes() > 0 {
				t.Logf("connection %s after the download started: %v", time.Since(start).Round(time.Millisecond), c)
				break
			}
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("no download to %s 5 s after it started; last: %v", destination, got)
		}
	}
}
