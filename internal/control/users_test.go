package control

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/control/controltest"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/health"
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

func TestUserService(t *testing.T) {
	users := controltest.NewUsers(t, "eu", "nl")
	ops, _ := userClients(newUserSource(users, daemon.Status{}))
	ctx := t.Context()

	// AddDevice creates the user; no device is the default one.
	phone, err := ops.AddDevice(ctx, &lotsmanv1.AddDeviceRequest{User: "alice", Device: "phone", Profile: "eu"})
	if err != nil {
		t.Fatal(err)
	}
	if phone.PeerId != "p_1" || phone.Config != "config of p_1" || users.Applied() != 1 {
		t.Errorf("AddDevice = %v, applies %d", phone, users.Applied())
	}
	if _, err := ops.AddDevice(ctx, &lotsmanv1.AddDeviceRequest{User: "alice", Profile: "nl"}); err != nil {
		t.Fatal(err)
	}
	ops.AddDevice(ctx, &lotsmanv1.AddDeviceRequest{User: "alice", Device: "phone", Profile: "nl"})
	ops.AddDevice(ctx, &lotsmanv1.AddDeviceRequest{User: "bob", Device: "laptop", Profile: "eu"})
	for _, tc := range []struct {
		req  *lotsmanv1.AddDeviceRequest
		code connect.Code
	}{
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: "phone", Profile: "eu"}, connect.CodeAlreadyExists},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: "phone", Profile: "us"}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{Device: "phone", Profile: "eu"}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: "phone"}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: "pho\nne", Profile: "eu"}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: " phone", Profile: "eu"}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: strings.Repeat("x", maxNameBytes+1), Profile: "eu"}, connect.CodeInvalidArgument},
		{&lotsmanv1.AddDeviceRequest{User: "alice", Device: "\xff", Profile: "eu"}, connect.CodeInvalidArgument},
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
	created := func(u *lotsmanv1.User) {
		u.Created = nil
		for _, d := range u.Devices {
			for _, p := range d.Peers {
				p.Created = nil
			}
		}
	}
	for _, u := range list.Users {
		created(u)
	}
	want := []*lotsmanv1.User{
		{Name: "alice", Devices: []*lotsmanv1.Device{
			{Name: "default", Peers: []*lotsmanv1.DevicePeer{{PeerId: "p_2", Profile: "nl", Address: "10.77.0.3"}}},
			{Name: "phone", Peers: []*lotsmanv1.DevicePeer{
				{PeerId: "p_1", Profile: "eu", Address: "10.77.0.2"}, {PeerId: "p_3", Profile: "nl", Address: "10.77.0.4"}}},
		}},
		{Name: "bob", HasLink: true, Devices: []*lotsmanv1.Device{
			{Name: "laptop", Peers: []*lotsmanv1.DevicePeer{{PeerId: "p_4", Profile: "eu", Address: "10.77.0.5"}}},
		}},
	}
	if diff := cmp.Diff(want, list.Users, protocmp.Transform()); diff != "" {
		t.Errorf("List (-want +got):\n%s", diff)
	}

	if c, err := ops.GetConfig(ctx, &lotsmanv1.GetConfigRequest{PeerId: "p_4"}); err != nil || c.Config != "config of p_4" {
		t.Errorf("GetConfig(p_4) = %v, %v", c, err)
	}
	for _, id := range []string{"p_99", "", "p_", "p_0", "p_-1", "p_01", "4", "x_4", "p_4 "} {
		_, err := ops.GetConfig(ctx, &lotsmanv1.GetConfigRequest{PeerId: id})
		wantCode(t, "GetConfig "+id, err, connect.CodeNotFound)
	}

	// RemoveDevice: one profile, or every profile when none is named.
	if _, err := ops.RemoveDevice(ctx, &lotsmanv1.RemoveDeviceRequest{User: "alice", Device: "phone", Profile: "nl"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.RemoveDevice(ctx, &lotsmanv1.RemoveDeviceRequest{User: "alice"}); err != nil {
		t.Fatal(err)
	}
	if peers, _ := users.St.UserPeers("alice"); len(peers) != 1 || peers[0].Device != "phone" || peers[0].Profile != "eu" {
		t.Errorf("alice's peers after removals: %+v", peers)
	}
	_, err = ops.RemoveDevice(ctx, &lotsmanv1.RemoveDeviceRequest{User: "alice", Device: "tablet"})
	wantCode(t, "RemoveDevice of a missing device", err, connect.CodeNotFound)

	if _, err := ops.RevokeLink(ctx, &lotsmanv1.RevokeLinkRequest{User: "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.St.ResolveLink(token.Token); err == nil {
		t.Error("the revoked link still resolves")
	}
	_, err = ops.IssueLink(ctx, &lotsmanv1.IssueLinkRequest{User: "carol"})
	wantCode(t, "IssueLink for a missing user", err, connect.CodeNotFound)
	_, err = ops.RevokeLink(ctx, &lotsmanv1.RevokeLinkRequest{User: "carol"})
	wantCode(t, "RevokeLink for a missing user", err, connect.CodeNotFound)

	before := users.Applied()
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
	users := controltest.NewUsers(t, "eu")
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
			_, err = self.AddDevice(ctx, &lotsmanv1.AddSelfDeviceRequest{Device: "phone", Profile: "eu"})
			wantCode(t, "AddDevice", err, connect.CodeUnauthenticated)
			_, err = self.RemoveDevice(ctx, &lotsmanv1.RemoveSelfDeviceRequest{Device: "phone"})
			wantCode(t, "RemoveDevice", err, connect.CodeUnauthenticated)
			_, err = self.GetConfig(ctx, &lotsmanv1.GetConfigRequest{PeerId: "p_1"})
			wantCode(t, "GetConfig", err, connect.CodeUnauthenticated)
			_, err = self.ListProfiles(ctx, &lotsmanv1.ListProfilesRequest{})
			wantCode(t, "ListProfiles", err, connect.CodeUnauthenticated)
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
	users := controltest.NewUsers(t, "eu", "nl")
	ops, self := userClients(newUserSource(users, daemon.Status{}))
	alice, bob := as(t.Context(), "alice"), as(t.Context(), "bob")
	bobsPeer, err := ops.AddDevice(t.Context(), &lotsmanv1.AddDeviceRequest{User: "bob", Device: "phone", Profile: "eu"})
	if err != nil {
		t.Fatal(err)
	}
	users.St.CreateUser("alice")

	phone, err := self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: "phone", Profile: "eu"})
	if err != nil {
		t.Fatal(err)
	}
	if peer, _ := users.St.PeerByID(2); phone.PeerId != "p_2" || phone.Config != "config of p_2" || peer.User != "alice" {
		t.Errorf("AddDevice = %v, peer %+v", phone, peer)
	}
	_, err = self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: "phone", Profile: "eu"})
	wantCode(t, "AddDevice twice", err, connect.CodeAlreadyExists)
	_, err = self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Profile: "eu"})
	wantCode(t, "AddDevice without a device", err, connect.CodeInvalidArgument)
	_, err = self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: "tablet", Profile: "us"})
	wantCode(t, "AddDevice with a missing profile", err, connect.CodeInvalidArgument)

	got, err := self.Get(alice, &lotsmanv1.GetSelfRequest{})
	if err != nil || got.Name != "alice" || len(got.Devices) != 1 || got.Devices[0].Peers[0].PeerId != "p_2" {
		t.Errorf("Get = %v, %v", got, err)
	}

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
	_, err = self.RemoveDevice(bob, &lotsmanv1.RemoveSelfDeviceRequest{Device: "phone", Profile: "nl"})
	wantCode(t, "RemoveDevice of a profile bob's phone lacks", err, connect.CodeNotFound)
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

	profiles, err := self.ListProfiles(alice, &lotsmanv1.ListProfilesRequest{})
	wantProfiles := &lotsmanv1.ListProfilesResponse{Profiles: []*lotsmanv1.Profile{{Name: "eu"}, {Name: "nl"}}}
	if err != nil || !cmp.Equal(profiles, wantProfiles, protocmp.Transform()) {
		t.Errorf("ListProfiles = %v, %v", profiles, err)
	}
}

func TestUserSelfMaxDevices(t *testing.T) {
	users := controltest.NewUsers(t, "eu", "nl")
	users.Limit = 2
	users.St.CreateUser("alice")
	_, self := userClients(newUserSource(users, daemon.Status{}))
	alice := as(t.Context(), "alice")
	for _, d := range []string{"phone", "laptop"} {
		if _, err := self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: d, Profile: "eu"}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: "tablet", Profile: "eu"})
	wantCode(t, "a third device", err, connect.CodeResourceExhausted)
	// Another profile is no new device: switching profiles stays possible.
	if _, err := self.AddDevice(alice, &lotsmanv1.AddSelfDeviceRequest{Device: "phone", Profile: "nl"}); err != nil {
		t.Errorf("another profile on a device at the limit: %v", err)
	}
	// The operator is not limited.
	ops, _ := userClients(newUserSource(users, daemon.Status{}))
	if _, err := ops.AddDevice(t.Context(), &lotsmanv1.AddDeviceRequest{User: "alice", Device: "tablet", Profile: "eu"}); err != nil {
		t.Errorf("operator past the limit: %v", err)
	}

	// Concurrent adds cannot pass the limit together.
	users.St.CreateUser("bob")
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			self.AddDevice(as(t.Context(), "bob"), &lotsmanv1.AddSelfDeviceRequest{Device: "d" + strconv.Itoa(i), Profile: "eu"})
		})
	}
	wg.Wait()
	if peers, _ := users.St.UserPeers("bob"); len(peers) != 2 {
		t.Errorf("concurrent adds left bob %d devices, want 2", len(peers))
	}
}

func TestUserSelfAddRateLimit(t *testing.T) {
	users := controltest.NewUsers(t, "eu")
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
		_, err := self.AddDevice(as(t.Context(), user), &lotsmanv1.AddSelfDeviceRequest{Device: device, Profile: "eu"})
		return err
	}
	for i := range AddBurst {
		if err := add("alice", "d"+strconv.Itoa(i)); err != nil {
			t.Fatalf("add %d of the burst: %v", i+1, err)
		}
	}
	wantCode(t, "an add past the burst", add("alice", "extra"), connect.CodeResourceExhausted)
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
			{Name: "nl", Kind: "awg", Geo: "NL", Tags: []string{"eu"}, State: health.Up, Routed: 2, Active: 2, Latency: time.Millisecond},
			{Name: "de", Kind: "awg", Geo: "DE", Tags: []string{"eu"}, State: health.Up, Routed: 1, Active: 1},
			{Name: "us", Kind: "awg", Geo: "US", State: health.Up, Routed: 1},
			{Name: "fi", Kind: "awg", Geo: "FI", State: health.Up},
		},
		Peers: []daemon.PeerStatus{
			{ID: 1, User: "alice", Device: "phone", Profile: "nl", Upstream: "nl"},
			{ID: 2, User: "alice", Device: "laptop", Profile: "gone", Upstream: "fi"}, // a profile since removed
			{ID: 3, User: "bob", Device: "phone", Profile: "eu", Upstream: "de"},
			{ID: 4, User: "bob", Device: "laptop", Profile: "any", Upstream: "us"},
		},
	}
	profiles := map[string]config.Profile{
		"nl":  {Name: "nl", Prefer: []config.Match{{Geo: "NL"}}},
		"eu":  {Name: "eu", Prefer: []config.Match{{Tag: "eu"}}},
		"any": {Name: "any"},
	}
	tr := trafficSnapshot(now, map[int64]uint64{1: 10, 2: 20, 3: 300, 4: 4000}, map[string]uint64{"nl": 5000, "fi": 6000})
	got := userSnapshot(now, 1280, st, tr, "alice", profiles)
	want := &lotsmanv1.StatusSnapshot{
		Time: got.Time, ClientMtu: 1280,
		Total: &lotsmanv1.Traffic{DownBytes: 30, UpBytes: 30, DownRate: 30, UpRate: 30},
		Upstreams: []*lotsmanv1.UpstreamStatus{
			{Name: "nl", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_AWG, Geo: "NL", Tags: []string{"eu"},
				State: lotsmanv1.UpstreamState_UPSTREAM_STATE_UP, Latency: got.Upstreams[0].Latency},
			// Routed through, though no profile of alice's names it.
			{Name: "fi", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_AWG, Geo: "FI", State: lotsmanv1.UpstreamState_UPSTREAM_STATE_UP},
		},
		Peers: []*lotsmanv1.PeerStatus{
			{PeerId: "p_1", User: "alice", Device: "phone", Profile: "nl", Upstream: "nl", Traffic: &lotsmanv1.Traffic{DownBytes: 10, UpBytes: 10, DownRate: 10, UpRate: 10}},
			{PeerId: "p_2", User: "alice", Device: "laptop", Profile: "gone", Upstream: "fi", Traffic: &lotsmanv1.Traffic{DownBytes: 20, UpBytes: 20, DownRate: 20, UpRate: 20}},
		},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("alice's snapshot (-want +got):\n%s", diff)
	}

	// bob's "any" profile may use every upstream; nothing of alice's shows.
	got = userSnapshot(now, 1280, st, tr, "bob", profiles)
	var names []string
	for _, u := range got.Upstreams {
		names = append(names, u.Name)
		if u.Routed != 0 || u.Active != 0 || u.Traffic != nil {
			t.Errorf("bob sees %s's routed %d, active %d, traffic %v", u.Name, u.Routed, u.Active, u.Traffic)
		}
	}
	if !cmp.Equal(names, []string{"nl", "de", "us", "fi"}) || len(got.Peers) != 2 || got.Total.DownBytes != 4300 {
		t.Errorf("bob's snapshot: upstreams %v, peers %v, total %v", names, got.Peers, got.Total)
	}

	// A user without peers sees no upstream; before the first sample, no total.
	got = userSnapshot(now, 1280, st, traffic.Snapshot{}, "carol", profiles)
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
	users := controltest.NewUsers(t, "nl")
	users.St.CreateUser("alice")
	src := newUserSource(users, daemon.Status{
		Upstreams: []daemon.UpstreamStatus{{Name: "nl", Kind: "awg", Geo: "NL"}},
		Peers: []daemon.PeerStatus{
			{ID: 1, User: "alice", Device: "phone", Profile: "nl", Upstream: "nl"},
			{ID: 2, User: "bob", Device: "phone", Profile: "nl", Upstream: "nl"},
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
	if len(first.Peers) != 1 || first.Peers[0].User != "alice" || len(first.Upstreams) != 1 {
		t.Errorf("alice's first snapshot: %v", first)
	}
	users.St.DeleteUser("alice")
	_, err = stream.Receive()
	wantCode(t, "Watch after the user's removal", err, connect.CodeUnauthenticated)
}
