package web

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
)

const testOrigin = "https://vpn.example.com"

// fakeAuth signs in whoever the test says.
type fakeAuth struct {
	operator bool
	user     string
}

func (a fakeAuth) Operator(*http.Request) error {
	if !a.operator {
		return errors.New("no operator session")
	}
	return nil
}

func (a fakeAuth) User(*http.Request) (string, error) {
	if a.user == "" {
		return "", errors.New("no user link")
	}
	return a.user, nil
}

// fakeDaemon records what reaches the daemon and answers an empty message.
type fakeDaemon struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []string
}

func (d *fakeDaemon) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	d.mu.Lock()
	d.reqs, d.body = append(d.reqs, r), append(d.body, string(b))
	d.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/proto"}},
		Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
}

func (d *fakeDaemon) calls() []*http.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reqs
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testConfig(t *testing.T, extra string) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte("public_url: " + testOrigin + "\n" + extra))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func testPanel(t *testing.T, auth Authenticator, daemon http.RoundTripper) http.Handler {
	t.Helper()
	return New(Deps{Config: testConfig(t, ""), Auth: auth, Daemon: daemon, UI: fstest.MapFS{}, Log: discardLog()})
}

// rpc is a Connect unary call as a browser on the panel's origin sends it.
func rpc(procedure, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api"+procedure, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/proto")
	r.Header.Set("Connect-Protocol-Version", "1")
	r.Header.Set("Origin", testOrigin)
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// errorCode is the Connect error code of a unary response, "" on success.
func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code == http.StatusOK {
		return ""
	}
	var e struct{ Code string }
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("status %d, body %q: %v", w.Code, w.Body, err)
	}
	return e.Code
}

func TestAPIScopes(t *testing.T) {
	const (
		operatorRPC = lotsmanv1connect.ProfileServiceListProcedure
		userRPC     = lotsmanv1connect.UserSelfServiceGetProcedure
	)
	for _, tc := range []struct {
		name      string
		auth      Authenticator
		procedure string
		want      string
	}{
		{"nobody signed in: no operator", fakeAuth{}, operatorRPC, "unauthenticated"},
		{"nobody signed in: no user", fakeAuth{}, userRPC, "unauthenticated"},
		{"operator", fakeAuth{operator: true}, operatorRPC, ""},
		{"user", fakeAuth{user: "alice"}, userRPC, ""},
		{"a user is no operator", fakeAuth{user: "alice"}, operatorRPC, "unauthenticated"},
		{"an operator has no user", fakeAuth{operator: true}, userRPC, "unauthenticated"},
		{"internal", fakeAuth{operator: true, user: "alice"}, lotsmanv1connect.UserSelfServiceResolveLinkProcedure, "permission_denied"},
		{"no scope", fakeAuth{operator: true, user: "alice"}, "/lotsman.v1.StatusService/Nope", "permission_denied"},
		{"another package", fakeAuth{operator: true, user: "alice"}, "/grpc.health.v1.Health/Check", "permission_denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := &fakeDaemon{}
			w := serve(testPanel(t, tc.auth, daemon), rpc(tc.procedure, "x"))
			if got := errorCode(t, w); got != tc.want {
				t.Errorf("code %q, want %q", got, tc.want)
			}
			if reached := len(daemon.calls()) == 1; reached != (tc.want == "") {
				t.Errorf("daemon reached: %v", reached)
			}
		})
	}
}

// A scope-less RPC is refused even with every identity at hand: the table,
// not the authenticator, decides what a browser may call.
func TestAPIRefusesUnscopedRPC(t *testing.T) {
	daemon := &fakeDaemon{}
	a := newAPI(map[string]Scope{}, fakeAuth{operator: true, user: "alice"}, daemon, discardLog())
	r := rpc(lotsmanv1connect.StatusServiceWatchProcedure, "")
	if got := errorCode(t, serve(a, r)); got != "permission_denied" || len(daemon.calls()) != 0 {
		t.Errorf("code %q, daemon calls %d", got, len(daemon.calls()))
	}
}

func TestAPIPublicNeedsNoSignIn(t *testing.T) {
	daemon := &fakeDaemon{}
	a := newAPI(map[string]Scope{"/lotsman.v1.X/Y": Public}, fakeAuth{}, daemon, discardLog())
	if got := errorCode(t, serve(a, rpc("/lotsman.v1.X/Y", ""))); got != "" || len(daemon.calls()) != 1 {
		t.Errorf("code %q, daemon calls %d", got, len(daemon.calls()))
	}
}

// What the daemon gets: the procedure at its own path, the body, the
// protocol headers, and Lotsman-User only as the panel set it.
func TestAPIForwardsOnlyProtocolHeaders(t *testing.T) {
	browser := func(procedure string) *http.Request {
		r := rpc(procedure, "payload")
		r.Header.Set("Connect-Timeout-Ms", "5000")
		r.Header.Set("Lotsman-User", "mallory")
		r.Header["lotsman-user"] = []string{"mallory"} // as an HTTP/2 hop may pass it
		r.Header.Set("Lotsman-Anything", "x")
		r.Header.Set("Cookie", "session=secret")
		r.Header.Set("Authorization", "Bearer secret")
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
		r.Header.Set("Forwarded", "for=203.0.113.7")
		return r
	}
	for _, tc := range []struct {
		name, procedure, user string
		auth                  Authenticator
	}{
		{"operator", lotsmanv1connect.ProfileServiceListProcedure, "", fakeAuth{operator: true}},
		{"user", lotsmanv1connect.UserSelfServiceGetProcedure, "alice", fakeAuth{user: "alice"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := &fakeDaemon{}
			if w := serve(testPanel(t, tc.auth, daemon), browser(tc.procedure)); w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			calls := daemon.calls()
			if len(calls) != 1 {
				t.Fatalf("daemon calls = %d", len(calls))
			}
			got := calls[0]
			if got.URL.Path != tc.procedure || daemon.body[0] != "payload" {
				t.Errorf("path %q body %q", got.URL.Path, daemon.body[0])
			}
			want := http.Header{
				"Content-Type": {"application/proto"}, "Connect-Protocol-Version": {"1"}, "Connect-Timeout-Ms": {"5000"},
			}
			if tc.user != "" {
				want["Lotsman-User"] = []string{tc.user}
			}
			for name := range got.Header {
				if name == "User-Agent" || name == "Content-Length" {
					continue // the transport's own
				}
				if strings.Join(got.Header[name], ",") != strings.Join(want[name], ",") {
					t.Errorf("daemon got %s: %q, want %q", name, got.Header[name], want[name])
				}
			}
			for name := range want {
				if got.Header.Get(name) == "" {
					t.Errorf("daemon missing %s", name)
				}
			}
		})
	}
}

func TestAPIRequestLimits(t *testing.T) {
	daemon := &fakeDaemon{}
	h := testPanel(t, fakeAuth{operator: true}, daemon)
	big := rpc(lotsmanv1connect.ProfileServicePutProcedure, strings.Repeat("x", maxRequestBytes+1))
	if got := errorCode(t, serve(h, big)); got != "resource_exhausted" {
		t.Errorf("oversized body: code %q", got)
	}
	get := rpc(lotsmanv1connect.ProfileServiceListProcedure, "")
	get.Method = http.MethodGet
	if w := serve(h, get); w.Code == http.StatusOK || w.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET: status %d, Allow %q", w.Code, w.Header().Get("Allow"))
	}
	if n := len(daemon.calls()); n != 0 {
		t.Errorf("daemon calls = %d", n)
	}
	exact := rpc(lotsmanv1connect.ProfileServicePutProcedure, strings.Repeat("x", maxRequestBytes))
	if got := errorCode(t, serve(h, exact)); got != "" {
		t.Errorf("body at the limit: code %q", got)
	}
}

type downDaemon struct{}

func (downDaemon) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial unix /run/lotsman/control.sock: connect: permission denied")
}

func TestAPIDaemonUnreachable(t *testing.T) {
	w := serve(testPanel(t, fakeAuth{operator: true}, downDaemon{}), rpc(lotsmanv1connect.ProfileServiceListProcedure, ""))
	if got := errorCode(t, w); got != "unavailable" {
		t.Errorf("code %q", got)
	}
	if strings.Contains(w.Body.String(), "permission denied") {
		t.Errorf("the socket error reached the browser: %s", w.Body)
	}
}
