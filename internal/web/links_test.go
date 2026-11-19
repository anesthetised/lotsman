package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/control"
	"github.com/anesthetised/lotsman/internal/control/controltest"
	"github.com/anesthetised/lotsman/internal/policy"
)

// fakeResolver knows the links the test gives it and counts questions.
type fakeResolver struct {
	mu    sync.Mutex
	links map[string]string // token → user
	down  bool
	calls int
}

func (f *fakeResolver) Resolve(_ context.Context, token string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.down {
		return "", connect.NewError(connect.CodeUnavailable, "dial unix: no such file")
	}
	if user, ok := f.links[token]; ok {
		return user, nil
	}
	return "", connect.NewError(connect.CodeNotFound, "unknown or revoked link")
}

func (f *fakeResolver) set(token, user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if user == "" {
		delete(f.links, token)
	} else {
		f.links[token] = user
	}
}

func (f *fakeResolver) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Tokens of the issued form.
var (
	aliceToken = strings.Repeat("a", 42) + "A"
	bobToken   = strings.Repeat("b", 42) + "A"
	otherToken = strings.Repeat("c", 42) + "A"
)

// clock is a settable time.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func testLinks(r Resolver) (*Links, *clock) {
	l := NewLinks(r)
	c := &clock{now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	l.now = c.Now
	return l, c
}

// linkRequest is a user call from addr with the given Authorization.
func linkRequest(addr, authorization string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api"+lotsmanv1connect.UserSelfServiceGetProcedure, nil)
	r.RemoteAddr = addr
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	return r.WithContext(context.WithValue(r.Context(), clientKey{}, clientAddr(r, nil)))
}

func TestLinksResolveAndCache(t *testing.T) {
	res := &fakeResolver{links: map[string]string{aliceToken: "alice"}}
	links, clk := testLinks(res)
	if user, err := links.User(linkRequest("192.0.2.1:1", "Bearer "+aliceToken)); err != nil || user != "alice" {
		t.Fatalf("User = %q, %v", user, err)
	}
	// Cached: the daemon is not asked again within the TTL, from any address.
	clk.add(linkTTL - time.Second)
	if user, err := links.User(linkRequest("198.51.100.9:2", "Bearer "+aliceToken)); err != nil || user != "alice" || res.asked() != 1 {
		t.Errorf("cached User = %q, %v after %d questions", user, err, res.asked())
	}
	// Past it, the daemon decides again: a revoked link stops working.
	res.set(aliceToken, "")
	clk.add(time.Second)
	_, err := links.User(linkRequest("192.0.2.1:1", "Bearer "+aliceToken))
	if connect.CodeOf(err) != connect.CodeUnauthenticated || res.asked() != 2 {
		t.Errorf("revoked: %v after %d questions", err, res.asked())
	}
	// The failure dropped nothing that a later success depends on, and
	// the cache holds no failure.
	res.set(aliceToken, "alice")
	if user, err := links.User(linkRequest("192.0.2.1:1", "Bearer "+aliceToken)); err != nil || user != "alice" {
		t.Errorf("after re-issue: %q, %v", user, err)
	}

	// Without a link, or a malformed one, the daemon is not asked.
	before := res.asked()
	for _, h := range []string{"", "Basic " + aliceToken, "Bearer ", "bearer " + aliceToken, "Bearer short", "Bearer " + aliceToken + "x"} {
		if _, err := links.User(linkRequest("192.0.2.50:1", h)); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("Authorization %q: %v", h, err)
		}
	}
	if res.asked() != before {
		t.Errorf("malformed links reached the daemon")
	}
}

func TestLinksFailureLimit(t *testing.T) {
	res := &fakeResolver{links: map[string]string{aliceToken: "alice", bobToken: "bob"}}
	links, clk := testLinks(res)
	// alice's link is cached before her address goes bad.
	if _, err := links.User(linkRequest("192.0.2.1:1", "Bearer "+aliceToken)); err != nil {
		t.Fatal(err)
	}
	for i := range linkFailBurst {
		if _, err := links.User(linkRequest("192.0.2.1:1", "Bearer "+otherToken)); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
	asked := res.asked()
	_, err := links.User(linkRequest("192.0.2.1:1", "Bearer "+bobToken))
	if connect.CodeOf(err) != connect.CodeResourceExhausted || res.asked() != asked {
		t.Errorf("a limited address: %v, daemon asked %d more times", err, res.asked()-asked)
	}
	if user, err := links.User(linkRequest("192.0.2.1:1", "Bearer "+aliceToken)); err != nil || user != "alice" {
		t.Errorf("a cached link from a limited address: %q, %v", user, err)
	}
	if user, err := links.User(linkRequest("192.0.2.2:1", "Bearer "+bobToken)); err != nil || user != "bob" {
		t.Errorf("another address: %q, %v", user, err)
	}
	clk.add(linkFailEvery)
	if user, err := links.User(linkRequest("192.0.2.1:1", "Bearer "+bobToken)); err != nil || user != "bob" {
		t.Errorf("after the interval: %q, %v", user, err)
	}

	// IPv6 addresses count by /64.
	for range linkFailBurst {
		links.User(linkRequest("[2001:db8:1:2::1]:1", "Bearer "+otherToken))
	}
	if _, err := links.User(linkRequest("[2001:db8:1:2::ffff]:1", "Bearer "+otherToken)); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("another address in the /64: %v", err)
	}
	if _, err := links.User(linkRequest("[2001:db8:1:3::1]:1", "Bearer "+otherToken)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("the next /64: %v", err)
	}
}

// A daemon that does not answer is no failure of the client's.
func TestLinksDaemonDown(t *testing.T) {
	res := &fakeResolver{down: true}
	links, _ := testLinks(res)
	for range linkFailBurst + 1 {
		if _, err := links.User(linkRequest("192.0.2.1:1", "Bearer "+aliceToken)); connect.CodeOf(err) != connect.CodeUnavailable {
			t.Fatalf("daemon down: %v", err)
		}
	}
	res.mu.Lock()
	res.down, res.links = false, map[string]string{aliceToken: "alice"}
	res.mu.Unlock()
	if user, err := links.User(linkRequest("192.0.2.1:1", "Bearer "+aliceToken)); err != nil || user != "alice" {
		t.Errorf("after the daemon came back: %q, %v", user, err)
	}
}

// What reaches the daemon for a user's call: Lotsman-User as the link
// resolved, never the browser's own, and no Authorization.
func TestLinksForwarding(t *testing.T) {
	daemon := &fakeDaemon{}
	res := &fakeResolver{links: map[string]string{aliceToken: "alice"}}
	h := testPanel(t, WithLinks(fakeAuth{}, NewLinks(res)), daemon)
	r := rpc(lotsmanv1connect.UserSelfServiceGetProcedure, "")
	r.Header.Set("Authorization", "Bearer "+aliceToken)
	r.Header.Set("Lotsman-User", "bob")
	if got := errorCode(t, serve(h, r)); got != "" {
		t.Fatalf("code %q", got)
	}
	calls := daemon.calls()
	if len(calls) != 1 || calls[0].Header.Values("Lotsman-User")[0] != "alice" || len(calls[0].Header.Values("Lotsman-User")) != 1 ||
		calls[0].Header.Get("Authorization") != "" {
		t.Errorf("daemon got %v", calls[0].Header)
	}

	// A browser's Lotsman-User without a link gets nowhere; nor does a
	// link on an operator call.
	r = rpc(lotsmanv1connect.UserSelfServiceGetProcedure, "")
	r.Header.Set("Lotsman-User", "alice")
	if got := errorCode(t, serve(h, r)); got != "unauthenticated" {
		t.Errorf("Lotsman-User alone: code %q", got)
	}
	r = rpc(lotsmanv1connect.UserServiceListProcedure, "")
	r.Header.Set("Authorization", "Bearer "+aliceToken)
	if got := errorCode(t, serve(h, r)); got != "unauthenticated" {
		t.Errorf("a link on an operator call: code %q", got)
	}
	if len(daemon.calls()) != 1 {
		t.Errorf("refused calls reached the daemon")
	}
}

// Through the panel and the real control API: a user acts on their own
// devices only, and a revoked link also ends a Watch it opened.
func TestLinksEndToEnd(t *testing.T) {
	users := controltest.NewUsers(t)
	users.St.CreateUser("alice")
	token, err := users.St.IssueLink("alice")
	if err != nil {
		t.Fatal(err)
	}
	bobs, err := users.AddPeer("bob", "phone", policy.DefaultRoute(), true)
	if err != nil {
		t.Fatal(err)
	}
	socket := usersSocket(t, users)
	logs := &syncWriter{w: new(bytes.Buffer)}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	links := NewLinks(DaemonResolver(DaemonTransport(socket)))
	a := newAPI(Scopes, WithLinks(fakeAuth{}, links), DaemonTransport(socket), log)
	a.recheck = 50 * time.Millisecond
	srv := httptest.NewServer(withClient(nil, a))
	t.Cleanup(srv.Close)
	client := func(token string) lotsmanv1connect.UserSelfServiceClient {
		hc := &http.Client{Transport: bearer{token}}
		return lotsmanv1connect.NewUserSelfServiceClient(connect.NewClient(connecthttp.NewTransport(hc, srv.URL+"/api")))
	}
	alice := client(token)
	ctx := t.Context()

	if u, err := alice.Get(ctx, &lotsmanv1.GetSelfRequest{}); err != nil || u.Name != "alice" || !u.HasLink {
		t.Fatalf("Get = %v, %v", u, err)
	}
	dev, err := alice.AddDevice(ctx, &lotsmanv1.AddSelfDeviceRequest{Device: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := alice.GetConfig(ctx, &lotsmanv1.GetConfigRequest{PeerId: dev.PeerId}); err != nil || c.Config != dev.Config {
		t.Errorf("own config: %v, %v", c, err)
	}
	if _, err := alice.GetConfig(ctx, &lotsmanv1.GetConfigRequest{PeerId: control.PeerID(bobs.ID)}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("bob's config: %v, want not_found", err)
	}
	if _, err := client(otherToken).Get(ctx, &lotsmanv1.GetSelfRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("an unknown link: %v", err)
	}

	stream, err := alice.Watch(ctx, &lotsmanv1.WatchStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatal(err)
	}
	links.forget(sha256.Sum256([]byte(token))) // as if the TTL had passed
	if err := users.St.RevokeLink("alice"); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Receive(); err != nil {
				ended <- err
				return
			}
		}
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Error("the stream outlived its revoked link")
	}
	stream.Close()

	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), otherToken) {
		t.Errorf("the log holds a token:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "link no longer valid") {
		t.Errorf("the log does not show the ended stream:\n%s", logs.String())
	}
}

// bearer is the user's page: every call carries its link.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.String()
}

// Nothing the panel logs or answers about links holds a token, whatever
// becomes of the call.
func TestLinksNeverLogToken(t *testing.T) {
	logs := &syncWriter{w: new(bytes.Buffer)}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	res := &fakeResolver{links: map[string]string{aliceToken: "alice"}}
	h := New(Deps{Config: testConfig(t, ""), Auth: WithLinks(fakeAuth{}, NewLinks(res)), Daemon: downDaemon{}, UI: fstest.MapFS{}, Log: log})
	var bodies []string
	for _, token := range []string{aliceToken, otherToken, "Bearer-not-a-token", aliceToken + "x"} {
		r := rpc(lotsmanv1connect.UserSelfServiceGetProcedure, "")
		r.Header.Set("Authorization", "Bearer "+token)
		bodies = append(bodies, serve(h, r).Body.String())
	}
	res.mu.Lock()
	res.down = true
	res.mu.Unlock()
	r := rpc(lotsmanv1connect.UserSelfServiceWatchProcedure, "")
	r.Header.Set("Authorization", "Bearer "+otherToken)
	bodies = append(bodies, serve(h, r).Body.String())

	for _, token := range []string{aliceToken, otherToken} {
		if strings.Contains(logs.String(), token) {
			t.Errorf("the log holds a token:\n%s", logs.String())
		}
		for _, b := range bodies {
			if strings.Contains(b, token) {
				t.Errorf("a response holds a token: %s", b)
			}
		}
	}
	if logs.String() == "" {
		t.Error("nothing was logged, so the test proves nothing")
	}
}
