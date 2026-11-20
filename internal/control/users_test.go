package control

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"connectrpc.com/connect/v2/connectproto"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/control/controltest"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/history"
	"github.com/anesthetised/lotsman/internal/policy"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/traffic"
)

// userClients are both user services of one server on src.
func userClients(src Source) (lotsmanv1connect.UserServiceClient, lotsmanv1connect.UserSelfServiceClient) {
	c := connect.NewClient(connectinprocess.New(NewServer(src)))
	return lotsmanv1connect.NewUserServiceClient(c), lotsmanv1connect.NewUserSelfServiceClient(c)
}

// as is a context whose calls carry Lotsman-User, as the panel sets it.
func as(ctx context.Context, user string) context.Context {
	ctx, info := connect.NewClientContext(ctx)
	info.RequestHeader().Set(UserHeader, user)
	return ctx
}

func wantCode(t *testing.T, what string, err error, want connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != want {
		t.Errorf("%s: %v, want %v", what, err, want)
	}
}

// route is a route message through the countries in order, each one geo
// or geo:provider,provider.
func route(fastest, strict bool, countries ...string) *lotsmanv1.Route {
	r := &lotsmanv1.Route{Fastest: fastest, Strict: strict}
	for _, c := range countries {
		geo, providers, _ := strings.Cut(c, ":")
		m := &lotsmanv1.Country{Geo: geo}
		if providers != "" {
			m.Providers = strings.Split(providers, ",")
		}
		r.Countries = append(r.Countries, m)
	}
	return r
}

// defaultRoute is policy.DefaultRoute as a message.
var defaultRoute = route(true, true)

func TestUserService(t *testing.T) {
	users := controltest.NewUsers(t)
	ops, _ := userClients(newUserSource(users, daemon.Status{}))
	ctx := t.Context()

	if u, err := ops.AddUser(ctx, &lotsmanv1.AddUserRequest{User: "carol"}); err != nil || u.Name != "carol" ||
		!cmp.Equal(u.Rights, &lotsmanv1.Rights{EditRoutes: true, ShowProviders: true}, protocmp.Transform()) {
		t.Errorf("AddUser = %v, %v; want carol with the default rights", u, err)
	}
	_, err := ops.AddUser(ctx, &lotsmanv1.AddUserRequest{User: "carol"})
	wantCode(t, "AddUser twice", err, connect.CodeAlreadyExists)
	for _, name := range []string{"", "a/b", " carol", "\xff"} {
		_, err := ops.AddUser(ctx, &lotsmanv1.AddUserRequest{User: name})
		wantCode(t, "AddUser "+name, err, connect.CodeInvalidArgument)
	}

	// AddDevice creates the user; without a route the device gets the default.
	phone, err := ops.AddDevice(ctx, &lotsmanv1.AddDeviceRequest{User: "alice", Device: "phone"})
	if err != nil {
		t.Fatal(err)
	}
	if phone.PeerId != "p_1" || phone.Config != "config of p_1" || users.Applied() != 1 {
		t.Errorf("AddDevice = %v, applies %d", phone, users.Applied())
	}
	if _, err := ops.AddDevice(ctx, &lotsmanv1.AddDeviceRequest{User: "alice", Device: "laptop", Route: route(false, true, "NL:beta", "DE")}); err != nil {
		t.Fatal(err)
	}
	ops.AddDevice(ctx, &lotsmanv1.AddDeviceRequest{User: "bob", Device: "laptop", Route: route(true, false, "NL")})
	for _, tc := range []struct {
		req  *lotsmanv1.AddDeviceRequest
		code connect.Code
	}{
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: "phone"}, connect.CodeAlreadyExists},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: "tablet", Route: route(false, true, "FR")}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: "tablet", Route: route(false, true, "DE:beta")}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: "tablet", Route: route(false, true, "NL", "NL")}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{Device: "phone"}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "a/b", Device: "phone"}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "alice"}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: "pho\nne"}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: " phone"}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: strings.Repeat("x", maxNameBytes+1)}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: "\xff"}, connect.CodeInvalidArgument},
	} {
		_, err := ops.AddDevice(ctx, tc.req)
		wantCode(t, "AddDevice "+tc.req.String(), err, tc.code)
	}

	token, err := ops.IssueLink(ctx, &lotsmanv1.IssueLinkRequest{User: "bob"})
	if err != nil || !store.ValidToken(token.Token) {
		t.Fatalf("IssueLink = %v, %v", token, err)
	}
	list, err := ops.List(ctx, &lotsmanv1.ListUsersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range list.Users {
		u.Created = nil
		for _, d := range u.Devices {
			d.Created = nil
		}
	}
	rights := &lotsmanv1.Rights{EditRoutes: true, ShowProviders: true}
	want := []*lotsmanv1.User{
		{Name: "alice", Rights: rights, Devices: []*lotsmanv1.Device{
			{PeerId: "p_2", Name: "laptop", Address: "10.77.0.3", Route: route(false, true, "NL:beta", "DE")},
			{PeerId: "p_1", Name: "phone", Address: "10.77.0.2", Route: defaultRoute},
		}},
		{Name: "bob", HasLink: true, Rights: rights, Devices: []*lotsmanv1.Device{
			{PeerId: "p_3", Name: "laptop", Address: "10.77.0.4", Route: route(true, false, "NL")},
		}},
		{Name: "carol", Rights: rights},
	}
	if diff := cmp.Diff(want, list.Users, protocmp.Transform()); diff != "" {
		t.Errorf("List (-want +got):\n%s", diff)
	}

	// SetRoute keeps the device, so its config; the route is checked as on AddDevice.
	before := users.Applied()
	d, err := ops.SetRoute(ctx, &lotsmanv1.SetRouteRequest{User: "alice", Device: "phone", Route: route(false, false, "DE")})
	if err != nil || d.PeerId != "p_1" || d.Address != "10.77.0.2" || !cmp.Equal(d.Route, route(false, false, "DE"), protocmp.Transform()) || users.Applied() != before+1 {
		t.Errorf("SetRoute = %v, %v", d, err)
	}
	for _, tc := range []struct {
		req  *lotsmanv1.SetRouteRequest
		code connect.Code
	}{
		{&lotsmanv1.SetRouteRequest{User: "alice", Device: "phone", Route: route(false, true, "FR")}, connect.CodeInvalidArgument},
		{&lotsmanv1.SetRouteRequest{User: "alice", Device: "phone"}, connect.CodeInvalidArgument},
		{&lotsmanv1.SetRouteRequest{User: "alice", Device: "tablet", Route: defaultRoute}, connect.CodeNotFound},
		{&lotsmanv1.SetRouteRequest{User: "dave", Device: "phone", Route: defaultRoute}, connect.CodeNotFound},
	} {
		_, err := ops.SetRoute(ctx, tc.req)
		wantCode(t, "SetRoute "+tc.req.String(), err, tc.code)
	}

	// SetRights replaces them whole; new routes must then use the exits.
	got, err := ops.SetRights(ctx, &lotsmanv1.SetRightsRequest{User: "alice", Rights: &lotsmanv1.Rights{ShowProviders: true, Exits: []string{"nl-be"}}})
	if err != nil || got.Rights.EditRoutes || !got.Rights.ShowProviders || !cmp.Equal(got.Rights.Exits, []string{"nl-be"}) || len(got.Devices) != 2 {
		t.Errorf("SetRights = %v, %v", got, err)
	}
	if u, _ := users.St.User("alice"); !cmp.Equal(u.Rights, store.Rights{ShowProviders: true, Exits: []string{"nl-be"}}) {
		t.Errorf("stored rights = %+v", u.Rights)
	}
	for _, tc := range []struct {
		req  *lotsmanv1.SetRightsRequest
		code connect.Code
	}{
		{&lotsmanv1.SetRightsRequest{User: "alice", Rights: &lotsmanv1.Rights{Exits: []string{"fr-ac"}}}, connect.CodeInvalidArgument},
		{&lotsmanv1.SetRightsRequest{User: "alice", Rights: &lotsmanv1.Rights{Exits: []string{"nl-be", "nl-be"}}}, connect.CodeInvalidArgument},
		{&lotsmanv1.SetRightsRequest{User: "alice"}, connect.CodeInvalidArgument},
		{&lotsmanv1.SetRightsRequest{User: "dave", Rights: &lotsmanv1.Rights{}}, connect.CodeNotFound},
	} {
		_, err := ops.SetRights(ctx, tc.req)
		wantCode(t, "SetRights "+tc.req.String(), err, tc.code)
	}
	_, err = ops.SetRoute(ctx, &lotsmanv1.SetRouteRequest{User: "alice", Device: "phone", Route: route(false, true, "DE")})
	wantCode(t, "SetRoute outside the exits", err, connect.CodeInvalidArgument)
	_, err = ops.AddDevice(ctx, &lotsmanv1.AddDeviceRequest{User: "alice", Device: "tablet", Route: route(false, true, "NL:acme")})
	wantCode(t, "AddDevice outside the exits", err, connect.CodeInvalidArgument)
	if _, err := ops.AddDevice(ctx, &lotsmanv1.AddDeviceRequest{User: "alice", Device: "tablet", Route: route(false, true, "NL")}); err != nil {
		t.Errorf("AddDevice within the exits: %v", err)
	}

	if c, err := ops.GetConfig(ctx, &lotsmanv1.GetConfigRequest{PeerId: "p_3"}); err != nil || c.Config != "config of p_3" {
		t.Errorf("GetConfig(p_3) = %v, %v", c, err)
	}
	for _, id := range []string{"p_99", "", "p_", "p_0", "p_-1", "p_01", "3", "x_3", "p_3 "} {
		_, err := ops.GetConfig(ctx, &lotsmanv1.GetConfigRequest{PeerId: id})
		wantCode(t, "GetConfig "+id, err, connect.CodeNotFound)
	}

	if _, err := ops.RemoveDevice(ctx, &lotsmanv1.RemoveDeviceRequest{User: "alice", Device: "phone"}); err != nil {
		t.Fatal(err)
	}
	if peers, _ := users.St.UserPeers("alice"); len(peers) != 2 || peers[0].Device != "laptop" || peers[1].Device != "tablet" {
		t.Errorf("alice's peers after the removal: %+v", peers)
	}
	_, err = ops.RemoveDevice(ctx, &lotsmanv1.RemoveDeviceRequest{User: "alice", Device: "phone"})
	wantCode(t, "RemoveDevice of a missing device", err, connect.CodeNotFound)

	if _, err := ops.RevokeLink(ctx, &lotsmanv1.RevokeLinkRequest{User: "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.St.ResolveLink(token.Token); err == nil {
		t.Error("the revoked link still resolves")
	}
	_, err = ops.IssueLink(ctx, &lotsmanv1.IssueLinkRequest{User: "dave"})
	wantCode(t, "IssueLink for a missing user", err, connect.CodeNotFound)
	_, err = ops.RevokeLink(ctx, &lotsmanv1.RevokeLinkRequest{User: "dave"})
	wantCode(t, "RevokeLink for a missing user", err, connect.CodeNotFound)

	before = users.Applied()
	if _, err := ops.RemoveUser(ctx, &lotsmanv1.RemoveUserRequest{User: "bob"}); err != nil {
		t.Fatal(err)
	}
	if users.Applied() != before+1 {
		t.Error("RemoveUser did not apply")
	}
	_, err = ops.RemoveUser(ctx, &lotsmanv1.RemoveUserRequest{User: "bob"})
	wantCode(t, "RemoveUser twice", err, connect.CodeNotFound)
}

// Every self-service RPC needs a user the store knows, in exactly one
// Lotsman-User header.
func TestUserSelfServiceNeedsUser(t *testing.T) {
	users := controltest.NewUsers(t)
	users.St.CreateUser("alice")
	_, self := userClients(newUserSource(users, daemon.Status{}))
	twice := func(ctx context.Context) context.Context {
		ctx, info := connect.NewClientContext(ctx)
		info.RequestHeader().Add(UserHeader, "alice")
		info.RequestHeader().Add(UserHeader, "alice")
		return ctx
	}
	for name, ctx := range map[string]context.Context{
		"no header":    t.Context(),
		"empty":        as(t.Context(), ""),
		"unknown user": as(t.Context(), "mallory"),
		"two headers":  twice(t.Context()),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := self.Get(ctx, &lotsmanv1.GetSelfRequest{})
			wantCode(t, "Get", err, connect.CodeUnauthenticated)
			_, err = self.AddDevice(ctx, &lotsmanv1.AddSelfDeviceRequest{Device: "phone"})
			wantCode(t, "AddDevice", err, connect.CodeUnauthenticated)
			_, err = self.SetRoute(ctx, &lotsmanv1.SetSelfRouteRequest{Device: "phone", Route: defaultRoute})
			wantCode(t, "SetRoute", err, connect.CodeUnauthenticated)
			_, err = self.RemoveDevice(ctx, &lotsmanv1.RemoveSelfDeviceRequest{Device: "phone"})
			wantCode(t, "RemoveDevice", err, connect.CodeUnauthenticated)
			_, err = self.GetConfig(ctx, &lotsmanv1.GetConfigRequest{PeerId: "p_1"})
			wantCode(t, "GetConfig", err, connect.CodeUnauthenticated)
			stream, err := self.Watch(ctx, &lotsmanv1.WatchStatusRequest{})
			if err == nil {
				_, err = stream.Receive()
			}
			wantCode(t, "Watch", err, connect.CodeUnauthenticated)
		})
	}
	if peers, _ := users.St.ListPeers(); len(peers) != 0 {
		t.Errorf("an unauthenticated call added peers: %+v", peers)
	}
}

func newUserSource(users *controltest.Users, st daemon.Status) *fakeSource {
	src := newFakeSource(st)
	src.Users = users
	return src
}

func TestUserSelfService(t *testing.T) {
	users := controltest.NewUsers(t)
	ops, self := userClients(newUserSource(users, daemon.Status{}))
	alice, bob := as(t.Context(), "alice"), as(t.Context(), "bob")
	bobsPeer, err := ops.AddDevice(t.Context(), &lotsmanv1.AddDeviceRequest{User: "bob", Device: "phone"})
	if err != nil {
		t.Fatal(err)
	}
	users.St.CreateUser("alice")

	phone, err := self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: "phone"})
	if err != nil {
		t.Fatal(err)
	}
	if peer, _ := users.St.PeerByID(2); phone.PeerId != "p_2" || phone.Config != "config of p_2" || peer.User != "alice" ||
		!cmp.Equal(peer.Route, policy.DefaultRoute()) {
		t.Errorf("AddDevice = %v, peer %+v", phone, peer)
	}
	if _, err := self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: "laptop", Route: route(false, true, "NL:acme")}); err != nil {
		t.Errorf("AddDevice with a route: %v", err)
	}
	_, err = self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: "phone"})
	wantCode(t, "AddDevice twice", err, connect.CodeAlreadyExists)
	_, err = self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{})
	wantCode(t, "AddDevice without a device", err, connect.CodeInvalidArgument)
	_, err = self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: "tablet", Route: route(false, true, "FR")})
	wantCode(t, "AddDevice with a country without upstreams", err, connect.CodeInvalidArgument)

	got, err := self.Get(alice, &lotsmanv1.GetSelfRequest{})
	if err != nil || got.Name != "alice" || len(got.Devices) != 2 || got.Devices[1].PeerId != "p_2" || !got.Rights.EditRoutes {
		t.Errorf("Get = %v, %v", got, err)
	}

	d, err := self.SetRoute(alice, &lotsmanv1.SetSelfRouteRequest{Device: "phone", Route: route(false, false, "DE", "NL:beta")})
	if err != nil || d.PeerId != "p_2" || !cmp.Equal(d.Route, route(false, false, "DE", "NL:beta"), protocmp.Transform()) {
		t.Errorf("SetRoute = %v, %v", d, err)
	}
	_, err = self.SetRoute(bob, &lotsmanv1.SetSelfRouteRequest{Device: "laptop", Route: defaultRoute})
	wantCode(t, "SetRoute of alice's device as bob", err, connect.CodeNotFound)
	_, err = self.SetRoute(alice, &lotsmanv1.SetSelfRouteRequest{Device: "phone"})
	wantCode(t, "SetRoute without a route", err, connect.CodeInvalidArgument)

	if c, err := self.GetConfig(alice, &lotsmanv1.GetConfigRequest{PeerId: "p_2"}); err != nil || c.Config != "config of p_2" {
		t.Errorf("own GetConfig = %v, %v", c, err)
	}
	// Another user's peer and no peer at all get the same answer.
	_, other := self.GetConfig(alice, &lotsmanv1.GetConfigRequest{PeerId: bobsPeer.PeerId})
	_, missing := self.GetConfig(alice, &lotsmanv1.GetConfigRequest{PeerId: "p_99"})
	wantCode(t, "GetConfig of bob's peer", other, connect.CodeNotFound)
	if other == nil || missing == nil || other.Error() != missing.Error() {
		t.Errorf("bob's peer: %v; no peer: %v; want the same error", other, missing)
	}
	// Removing names the device within the acting user only.
	_, err = self.RemoveDevice(bob, &lotsmanv1.RemoveSelfDeviceRequest{Device: "laptop"})
	wantCode(t, "RemoveDevice of a device bob lacks", err, connect.CodeNotFound)
	_, err = self.RemoveDevice(alice, &lotsmanv1.RemoveSelfDeviceRequest{})
	wantCode(t, "RemoveDevice without a device", err, connect.CodeInvalidArgument)
	if _, err := self.RemoveDevice(alice, &lotsmanv1.RemoveSelfDeviceRequest{Device: "phone"}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.St.PeerByID(1); err != nil {
		t.Error("alice's RemoveDevice removed bob's phone")
	}
	_, err = self.RemoveDevice(alice, &lotsmanv1.RemoveSelfDeviceRequest{Device: "phone"})
	wantCode(t, "RemoveDevice of a removed device", err, connect.CodeNotFound)
}

// The operator's rights decide which routes a user may set on their page.
func TestUserSelfRights(t *testing.T) {
	const allowed connect.Code = 0
	for _, tc := range []struct {
		name   string
		rights store.Rights
		route  *lotsmanv1.Route // for AddDevice and SetRoute; nil: AddDevice without one
		code   connect.Code     // of both; allowed for none
	}{
		{"default rights, no route", store.DefaultRights(), nil, allowed},
		{"default rights, providers", store.DefaultRights(), route(true, true, "NL:acme,beta"), allowed},
		{"no edit_routes, no route", store.Rights{ShowProviders: true}, nil, allowed},
		{"no edit_routes, a route", store.Rights{ShowProviders: true}, route(false, true, "NL"), connect.CodePermissionDenied},
		{"no edit_routes, the default route", store.Rights{ShowProviders: true}, defaultRoute, connect.CodePermissionDenied},
		{"no show_providers, countries", store.Rights{EditRoutes: true}, route(false, true, "NL", "DE"), allowed},
		{"no show_providers, a provider", store.Rights{EditRoutes: true}, route(false, true, "NL", "DE:acme"), connect.CodeInvalidArgument},
		{"exits, a country among them", store.Rights{EditRoutes: true, ShowProviders: true, Exits: []string{"de-ac"}}, route(false, false, "DE"), allowed},
		{"exits, a country outside them", store.Rights{EditRoutes: true, ShowProviders: true, Exits: []string{"de-ac"}}, route(false, false, "NL"), connect.CodeInvalidArgument},
		{"exits, a provider outside them", store.Rights{EditRoutes: true, ShowProviders: true, Exits: []string{"nl-ac"}}, route(false, false, "NL:beta"), connect.CodeInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			users := controltest.NewUsers(t)
			users.St.CreateUser("alice")
			if _, err := users.St.SetRights("alice", tc.rights); err != nil {
				t.Fatal(err)
			}
			users.St.AddPeer("alice", "phone", policy.DefaultRoute(), controltest.Subnet)
			_, self := userClients(newUserSource(users, daemon.Status{}))
			alice := as(t.Context(), "alice")

			check := func(what string, err error) {
				t.Helper()
				if tc.code == allowed && err != nil {
					t.Errorf("%s: %v, want it allowed", what, err)
				} else if tc.code != allowed {
					wantCode(t, what, err, tc.code)
				}
			}
			_, err := self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: "laptop", Route: tc.route})
			check("AddDevice", err)
			if err == nil && tc.route == nil {
				if p, _ := users.St.Peer("alice", "laptop"); !cmp.Equal(p.Route, policy.DefaultRoute()) {
					t.Errorf("a device added without a route has %v", p.Route)
				}
			}
			if tc.route != nil {
				_, err = self.SetRoute(alice, &lotsmanv1.SetSelfRouteRequest{Device: "phone", Route: tc.route})
				check("SetRoute", err)
			}
			if p, _ := users.St.Peer("alice", "phone"); tc.code != allowed && !cmp.Equal(p.Route, policy.DefaultRoute()) {
				t.Errorf("a refused SetRoute stored %v", p.Route)
			}
		})
	}
}

// Without show_providers a user's page shows countries only, even of a route
// the operator gave providers.
func TestUserSelfHidesProviders(t *testing.T) {
	users := controltest.NewUsers(t)
	users.St.CreateUser("alice")
	users.St.SetRights("alice", store.Rights{EditRoutes: true})
	providers := policy.Route{Strict: true, Countries: []policy.Country{{Geo: "NL", Providers: []string{"beta"}}, {Geo: "DE"}}}
	users.St.AddPeer("alice", "phone", providers, controltest.Subnet)
	_, self := userClients(newUserSource(users, daemon.Status{}))
	got, err := self.Get(as(t.Context(), "alice"), &lotsmanv1.GetSelfRequest{})
	if err != nil || len(got.Devices) != 1 || !cmp.Equal(got.Devices[0].Route, route(false, true, "NL", "DE"), protocmp.Transform()) {
		t.Errorf("Get = %v, %v; want the route without providers", got, err)
	}
	if p, _ := users.St.Peer("alice", "phone"); !cmp.Equal(p.Route, providers) {
		t.Errorf("Get changed the stored route: %v", p.Route)
	}
}

func TestUserSelfMaxDevices(t *testing.T) {
	users := controltest.NewUsers(t)
	users.Limit = 2
	users.St.CreateUser("alice")
	_, self := userClients(newUserSource(users, daemon.Status{}))
	alice := as(t.Context(), "alice")
	for _, d := range []string{"phone", "laptop"} {
		if _, err := self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: d}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: "tablet"})
	wantCode(t, "a third device", err, connect.CodeResourceExhausted)
	if l := limitOf(t, err); l.GetReason() != lotsmanv1.SelfServiceLimit_REASON_MAX_DEVICES || l.GetMaxDevices() != 2 {
		t.Errorf("a third device: detail %v, want max_devices 2", l)
	}
	// A new route is no new device.
	if _, err := self.SetRoute(alice, &lotsmanv1.SetSelfRouteRequest{Device: "phone", Route: route(false, true, "NL")}); err != nil {
		t.Errorf("a new route on a device at the limit: %v", err)
	}
	// The operator is not limited.
	ops, _ := userClients(newUserSource(users, daemon.Status{}))
	if _, err := ops.AddDevice(t.Context(), &lotsmanv1.AddDeviceRequest{User: "alice", Device: "tablet"}); err != nil {
		t.Errorf("operator past the limit: %v", err)
	}

	// Concurrent adds cannot pass the limit together.
	users.St.CreateUser("bob")
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			self.AddDevice(as(t.Context(), "bob"), &lotsmanv1.AddSelfDeviceRequest{Device: "d" + strconv.Itoa(i)})
		})
	}
	wg.Wait()
	if peers, _ := users.St.UserPeers("bob"); len(peers) != 2 {
		t.Errorf("concurrent adds left bob %d devices, want 2", len(peers))
	}
}

// max_devices 0 turns adding off; removing, routes and the operator still
// work.
func TestUserSelfMaxDevicesZero(t *testing.T) {
	users := controltest.NewUsers(t)
	users.Limit = 0
	ops, self := userClients(newUserSource(users, daemon.Status{}))
	if _, err := ops.AddDevice(t.Context(), &lotsmanv1.AddDeviceRequest{User: "alice", Device: "phone"}); err != nil {
		t.Fatalf("the operator with max_devices 0: %v", err)
	}
	alice := as(t.Context(), "alice")
	_, err := self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: "laptop"})
	wantCode(t, "a new device", err, connect.CodePermissionDenied)
	if peers, _ := users.St.UserPeers("alice"); len(peers) != 1 {
		t.Errorf("alice's peers: %+v", peers)
	}
	if _, err := self.SetRoute(alice, &lotsmanv1.SetSelfRouteRequest{Device: "phone", Route: route(false, true, "DE")}); err != nil {
		t.Errorf("a new route with max_devices 0: %v", err)
	}
	if _, err := self.RemoveDevice(alice, &lotsmanv1.RemoveSelfDeviceRequest{Device: "phone"}); err != nil {
		t.Errorf("removing with max_devices 0: %v", err)
	}
}

func TestUserSelfAddRateLimit(t *testing.T) {
	users := controltest.NewUsers(t)
	users.Limit = 100
	users.St.CreateUser("alice")
	users.St.CreateUser("bob")
	src := newUserSource(users, daemon.Status{})
	svc := newUserSelfService(src)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	s := connect.NewServer()
	lotsmanv1connect.RegisterUserSelfServiceHandler(s, svc)
	self := lotsmanv1connect.NewUserSelfServiceClient(connect.NewClient(connectinprocess.New(s)))
	add := func(user, device string) error {
		_, err := self.AddDevice(as(t.Context(), user), &lotsmanv1.AddSelfDeviceRequest{Device: device})
		return err
	}
	for i := range AddBurst {
		if err := add("alice", "d"+strconv.Itoa(i)); err != nil {
			t.Fatalf("add %d of the burst: %v", i+1, err)
		}
	}
	err := add("alice", "extra")
	wantCode(t, "an add past the burst", err, connect.CodeResourceExhausted)
	if l := limitOf(t, err); l.GetReason() != lotsmanv1.SelfServiceLimit_REASON_RATE_LIMITED || l.GetRetryAfter().AsDuration() != AddEvery {
		t.Errorf("an add past the burst: detail %v, want rate limited, retry after %s", l, AddEvery)
	}
	if err := add("bob", "phone"); err != nil {
		t.Errorf("alice's limit applied to bob: %v", err)
	}
	now = now.Add(AddEvery)
	if err := add("alice", "extra"); err != nil {
		t.Errorf("an add after the interval: %v", err)
	}
	wantCode(t, "a second add in the interval", add("alice", "extra2"), connect.CodeResourceExhausted)
}

func TestResolveLink(t *testing.T) {
	users := controltest.NewUsers(t)
	users.St.CreateUser("alice")
	token, _ := users.St.IssueLink("alice")
	_, self := userClients(newUserSource(users, daemon.Status{}))
	res, err := self.ResolveLink(t.Context(), &lotsmanv1.ResolveLinkRequest{Token: token})
	if err != nil || res.User != "alice" {
		t.Errorf("ResolveLink = %v, %v", res, err)
	}
	users.St.IssueLink("alice") // rotates
	_, err = self.ResolveLink(t.Context(), &lotsmanv1.ResolveLinkRequest{Token: token})
	wantCode(t, "a rotated token", err, connect.CodeNotFound)
	if err != nil && strings.Contains(err.Error(), token) {
		t.Errorf("the error repeats the token: %v", err)
	}
	_, err = self.ResolveLink(t.Context(), &lotsmanv1.ResolveLinkRequest{Token: "garbage"})
	wantCode(t, "garbage", err, connect.CodeNotFound)
}

func TestUserSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	st := daemon.Status{
		Upstreams: []daemon.UpstreamStatus{
			{Name: "nl", Kind: "awg", Geo: "NL", Provider: "acme", State: health.Up, Routed: 2, Active: 2, Latency: time.Millisecond,
				Speed: health.Speed{Down: 7, Up: 8, Time: now}, Incidents: []store.IncidentKind{store.IncidentSendErrors}},
			{Name: "de", Kind: "awg", Geo: "DE", Provider: "acme", State: health.Up, Routed: 1, Active: 1},
			{Name: "us", Kind: "awg", Geo: "US", Provider: "beta", State: health.Up, Routed: 1},
			{Name: "fi", Kind: "awg", Geo: "FI", Provider: "beta", State: health.Up},
		},
		Peers: []daemon.PeerStatus{
			{ID: 1, User: "alice", Device: "phone", Upstream: "nl"},
			{ID: 2, User: "alice", Device: "laptop", Upstream: "fi"}, // routed before her exits changed
			{ID: 3, User: "bob", Device: "phone", Upstream: "de"},
			{ID: 4, User: "bob", Device: "laptop", Upstream: "us"},
		},
	}
	alice := store.User{Name: "alice", Rights: store.Rights{EditRoutes: true, ShowProviders: true, Exits: []string{"nl", "gone"}}}
	tr := trafficSnapshot(now, map[int64]uint64{1: 10, 2: 20, 3: 300, 4: 4000}, map[string]uint64{"nl": 5000, "fi": 6000})
	got := userSnapshot(now, 1280, st, tr, alice)
	want := &lotsmanv1.StatusSnapshot{
		Time: got.Time, ClientMtu: 1280,
		Total: &lotsmanv1.Traffic{DownBytes: 30, UpBytes: 30, DownRate: 30, UpRate: 30},
		Upstreams: []*lotsmanv1.UpstreamStatus{
			// The exit's speed is hers to see; its incidents are the operator's.
			{Name: "nl", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_AWG, Geo: "NL", Provider: "acme",
				State: lotsmanv1.UpstreamState_UPSTREAM_STATE_UP, Latency: got.Upstreams[0].Latency,
				Speed: &lotsmanv1.Speed{DownRate: 7, UpRate: 8, Time: timestamppb.New(now)}},
			// Routed through, though not among her exits.
			{Name: "fi", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_AWG, Geo: "FI", Provider: "beta", State: lotsmanv1.UpstreamState_UPSTREAM_STATE_UP},
		},
		Peers: []*lotsmanv1.PeerStatus{
			{PeerId: "p_1", User: "alice", Device: "phone", Upstream: "nl", Traffic: &lotsmanv1.Traffic{DownBytes: 10, UpBytes: 10, DownRate: 10, UpRate: 10}},
			{PeerId: "p_2", User: "alice", Device: "laptop", Upstream: "fi", Traffic: &lotsmanv1.Traffic{DownBytes: 20, UpBytes: 20, DownRate: 20, UpRate: 20}},
		},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("alice's snapshot (-want +got):\n%s", diff)
	}

	// bob may use every upstream but sees no provider; nothing of alice's shows.
	bob := store.User{Name: "bob", Rights: store.Rights{EditRoutes: true}}
	got = userSnapshot(now, 1280, st, tr, bob)
	var names []string
	for _, u := range got.Upstreams {
		names = append(names, u.Name)
		if u.Routed != 0 || u.Active != 0 || u.Traffic != nil {
			t.Errorf("bob sees %s's routed %d, active %d, traffic %v", u.Name, u.Routed, u.Active, u.Traffic)
		}
		if u.Provider != "" {
			t.Errorf("bob sees %s's provider %q", u.Name, u.Provider)
		}
	}
	if !cmp.Equal(names, []string{"nl", "de", "us", "fi"}) || len(got.Peers) != 2 || got.Total.DownBytes != 4300 {
		t.Errorf("bob's snapshot: upstreams %v, peers %v, total %v", names, got.Peers, got.Total)
	}

	// A user with only stale exits and no peers sees no upstream; before
	// the first sample, no total.
	carol := store.User{Name: "carol", Rights: store.Rights{Exits: []string{"gone"}}}
	got = userSnapshot(now, 1280, st, traffic.Snapshot{}, carol)
	if len(got.Upstreams) != 0 || len(got.Peers) != 0 || got.Total != nil {
		t.Errorf("carol's snapshot: %v", got)
	}
}

// trafficSnapshot gives every peer and upstream the same value in every
// field, so a sum shows whose bytes went into it.
func trafficSnapshot(now time.Time, peers map[int64]uint64, upstreams map[string]uint64) traffic.Snapshot {
	tr := traffic.Snapshot{Time: now, Peers: map[int64]traffic.Traffic{}, Upstreams: map[string]traffic.Traffic{}}
	for id, v := range peers {
		tr.Peers[id] = traffic.Traffic{DownBytes: v, UpBytes: v, DownRate: v, UpRate: v}
	}
	for name, v := range upstreams {
		tr.Upstreams[name] = traffic.Traffic{DownBytes: v, UpBytes: v, DownRate: v, UpRate: v}
	}
	tr.Total = traffic.Traffic{DownBytes: 1 << 40} // the gateway's: never a user's
	return tr
}

// The user's Watch streams their view, and ends once they are removed.
func TestUserSelfWatch(t *testing.T) {
	users := controltest.NewUsers(t)
	users.St.CreateUser("alice")
	src := newUserSource(users, daemon.Status{
		Upstreams: []daemon.UpstreamStatus{{Name: "nl", Kind: "awg", Geo: "NL", Provider: "acme"}},
		Peers: []daemon.PeerStatus{
			{ID: 1, User: "alice", Device: "phone", Upstream: "nl"},
			{ID: 2, User: "bob", Device: "phone", Upstream: "nl"},
		},
	})
	_, self := userClients(src)
	stream, err := self.Watch(as(t.Context(), "alice"), &lotsmanv1.WatchStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Peers) != 1 || first.Peers[0].User != "alice" || len(first.Upstreams) != 1 || first.Upstreams[0].Provider != "acme" {
		t.Errorf("alice's first snapshot: %v", first)
	}
	// Rights apply to the next snapshot.
	users.St.SetRights("alice", store.Rights{EditRoutes: true})
	if next, err := stream.Receive(); err != nil || len(next.Upstreams) != 1 || next.Upstreams[0].Provider != "" {
		t.Errorf("the snapshot after hiding providers: %v, %v", next, err)
	}
	users.St.DeleteUser("alice")
	_, err = stream.Receive()
	wantCode(t, "Watch after the user's removal", err, connect.CodeUnauthenticated)
}

// A user's history covers the upstreams their Watch shows (their exits, and
// any a device of theirs is routed through), without bytes, and their own
// peers; anything else fails as a missing name does.
func TestUserSelfHistory(t *testing.T) {
	users := controltest.NewUsers(t)
	alices, _ := users.St.AddUserPeer("alice", "phone", policy.DefaultRoute(), controltest.Subnet)
	bobs, _ := users.St.AddUserPeer("bob", "phone", policy.DefaultRoute(), controltest.Subnet)
	users.St.SetRights("alice", store.Rights{Exits: []string{"nl"}})
	src := newUserSource(users, daemon.Status{
		Upstreams: []daemon.UpstreamStatus{{Name: "nl", Geo: "NL"}, {Name: "de", Geo: "DE"}, {Name: "us", Geo: "US"}},
		Peers: []daemon.PeerStatus{
			{ID: alices.ID, User: "alice", Upstream: "de"}, // routed outside her exits, as briefly after they change
			{ID: bobs.ID, User: "bob", Upstream: "us"},
		},
	})
	f := &fakeHistory{res: history.Result{Step: time.Minute, Samples: []store.Sample{
		{Time: t0, UpRatio: 0.5, Latency: 20 * time.Millisecond, Down: 1000, Up: 100, DownRate: 300, UpRate: 30},
	}}}
	src.HistorySource = f
	_, self := userClients(src)
	alice := as(t.Context(), "alice")
	req := func(name string) *lotsmanv1.HistoryRequest {
		return &lotsmanv1.HistoryRequest{Name: name, From: timestamppb.New(t0), To: timestamppb.New(t1)}
	}

	for _, name := range []string{"nl", "de"} {
		got, err := self.UpstreamHistory(alice, req(name))
		want := &lotsmanv1.HistoryResponse{Step: durationpb.New(time.Minute), Samples: []*lotsmanv1.Sample{
			{Time: timestamppb.New(t0), UpRatio: 0.5, Latency: durationpb.New(20 * time.Millisecond), DownRate: 300, UpRate: 30},
		}}
		if err != nil || f.name != name || !cmp.Equal(want, got, protocmp.Transform()) {
			t.Errorf("UpstreamHistory(%s) asked %q: %v, %v", name, f.name, got, err)
		}
	}
	peer := PeerID(alices.ID)
	got, err := self.PeerHistory(alice, req(peer))
	if err != nil || f.name != peer || len(got.Samples) != 1 || got.Samples[0].DownBytes != 1000 {
		t.Errorf("PeerHistory(own peer) = %v, %v", got, err)
	}

	// Another user's upstream or peer fails as a missing one does.
	f.err = store.ErrNotFound
	_, missingUpstream := self.UpstreamHistory(alice, req("gone"))
	_, missingPeer := self.PeerHistory(alice, req("p_99"))
	f.err = nil
	_, foreignUpstream := self.UpstreamHistory(alice, req("us"))
	_, foreignPeer := self.PeerHistory(alice, req(PeerID(bobs.ID)))
	_, unknownPeer := self.PeerHistory(alice, req("p_99")) // the store does not have it
	for what, err := range map[string]error{
		"a missing upstream": missingUpstream, "bob's upstream": foreignUpstream,
		"a missing peer": missingPeer, "bob's peer": foreignPeer, "a peer only the store lacks": unknownPeer,
	} {
		wantCode(t, what, err, connect.CodeNotFound)
		if err != nil && err.Error() != missingUpstream.Error() {
			t.Errorf("%s: %q, want %q", what, err, missingUpstream)
		}
	}
	_, err = self.PeerHistory(alice, req("alice"))
	wantCode(t, "a malformed peer id", err, connect.CodeNotFound)

	// The daemon judges the range first, for every name alike.
	f.err = fmt.Errorf("%w: too long", history.ErrRange)
	for _, name := range []string{"nl", "us"} {
		_, err := self.UpstreamHistory(alice, req(name))
		wantCode(t, "a bad range for "+name, err, connect.CodeInvalidArgument)
	}
	_, err = self.PeerHistory(alice, req(PeerID(bobs.ID)))
	wantCode(t, "a bad range for bob's peer", err, connect.CodeInvalidArgument)
	f.err = nil

	_, err = self.UpstreamHistory(alice, &lotsmanv1.HistoryRequest{})
	wantCode(t, "no name", err, connect.CodeInvalidArgument)
	_, err = self.PeerHistory(alice, &lotsmanv1.HistoryRequest{Name: peer, From: &timestamppb.Timestamp{Nanos: -1}})
	wantCode(t, "an invalid from", err, connect.CodeInvalidArgument)
	_, err = self.UpstreamHistory(t.Context(), req("nl"))
	wantCode(t, "no user", err, connect.CodeUnauthenticated)
	_, err = self.PeerHistory(as(t.Context(), "carol"), req(peer))
	wantCode(t, "an unknown user", err, connect.CodeUnauthenticated)
}

// limitOf is the SelfServiceLimit detail of err.
func limitOf(t *testing.T, err error) *lotsmanv1.SelfServiceLimit {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("%v is not a connect error", err)
	}
	for _, d := range cerr.Details() {
		if m, derr := connectproto.UnmarshalErrorDetail(d); derr == nil {
			if l, ok := m.(*lotsmanv1.SelfServiceLimit); ok {
				return l
			}
		}
	}
	t.Fatalf("%v has no SelfServiceLimit detail", err)
	return nil
}
