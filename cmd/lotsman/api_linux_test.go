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

// TestControlUsersAndConnections drives the user and connection services
// through the daemon's socket: a device added over the API with a route
// serves a client, a route no upstream serves is refused, and the client's
// download shows up as a live connection.
func TestControlUsersAndConnections(t *testing.T) {
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
	users := lotsmanv1connect.NewUserServiceClient(api)

	nlAcme := &lotsmanv1.Route{Strict: true, Countries: []*lotsmanv1.Country{{Geo: "NL", Providers: []string{"acme"}}}}
	added, err := users.AddDevice(t.Context(), &lotsmanv1.AddDeviceRequest{User: "alice", Device: "phone", Route: nlAcme})
	if err != nil {
		t.Fatal(err)
	}
	alice := newClient(t, added.Config)
	eventually(t, "alice exits via nl", 15*time.Second, func() bool {
		got, err := alice.exitVia(2 * time.Second)
		return err == nil && got == "nl"
	})
	// A route through a country no upstream serves is refused, the stored one stays.
	us := &lotsmanv1.Route{Strict: true, Countries: []*lotsmanv1.Country{{Geo: "US"}}}
	if _, err := users.SetRoute(t.Context(), &lotsmanv1.SetRouteRequest{User: "alice", Device: "phone", Route: us}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("set route us: %v, want invalid_argument", err)
	}
	list, err := users.List(t.Context(), &lotsmanv1.ListUsersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Users) != 1 || len(list.Users[0].Devices) != 1 || !proto.Equal(list.Users[0].Devices[0].Route, nlAcme) {
		t.Errorf("list = %v", list)
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
