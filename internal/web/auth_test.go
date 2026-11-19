package web

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
)

const testRPID = "vpn.example.com"

// newTestAuth is production's authenticator over an empty web.db.
func newTestAuth(t *testing.T, cfg *Config) *OperatorAuth {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a, err := NewOperatorAuth(cfg, db, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// authPanel is the whole panel with production's sign-in on a fake clock.
type authPanel struct {
	t      *testing.T
	h      http.Handler
	db     *sql.DB
	daemon *fakeDaemon
	now    time.Time
	// client is the address requests come from.
	client string
}

func newAuthPanel(t *testing.T) *authPanel {
	t.Helper()
	cfg := testConfig(t, "")
	a := newTestAuth(t, cfg)
	p := &authPanel{t: t, db: a.db, daemon: &fakeDaemon{}, now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), client: "192.0.2.1:1234"}
	a.now = func() time.Time { return p.now }
	p.h = New(Deps{Config: cfg, Auth: a, SignIn: a, Daemon: p.daemon, UI: fstest.MapFS{}, Log: discardLog()})
	return p
}

func (p *authPanel) advance(d time.Duration) { p.now = p.now.Add(d) }

// post sends a JSON body to the panel as its own page does.
func (p *authPanel) post(path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	p.t.Helper()
	raw, ok := body.([]byte)
	if !ok {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	r.RemoteAddr = p.client
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", testOrigin)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return serve(p.h, r)
}

// issue makes an enrollment link as `lotsman web enroll` does.
func (p *authPanel) issue() string {
	p.t.Helper()
	token, _, err := IssueEnrollment(p.t.Context(), p.db, p.now)
	if err != nil {
		p.t.Fatal(err)
	}
	return token
}

// enroll redeems token with key: the begin's response if it fails, else
// the finish's.
func (p *authPanel) enroll(key *softKey, token, name string) *httptest.ResponseRecorder {
	p.t.Helper()
	begin := p.post("/auth/enroll/begin", map[string]string{"token": token, "name": name}, nil)
	if begin.Code != http.StatusOK {
		return begin
	}
	return p.post("/auth/enroll/finish", key.create(p.t, begin.Body.Bytes()), nil)
}

func (p *authPanel) login(key *softKey) *httptest.ResponseRecorder {
	p.t.Helper()
	begin := p.post("/auth/login/begin", struct{}{}, nil)
	if begin.Code != http.StatusOK {
		return begin
	}
	return p.post("/auth/login/finish", key.get(p.t, begin.Body.Bytes()), nil)
}

// signedIn enrolls a new passkey and returns it with its session.
func (p *authPanel) signedIn(name string) (*softKey, *http.Cookie) {
	p.t.Helper()
	key := newSoftKey(p.t, testOrigin, testRPID)
	w := p.enroll(key, p.issue(), name)
	return key, sessionOf(p.t, w)
}

// call is an operator RPC with cookie: its Connect error code, "" on success.
func (p *authPanel) call(procedure string, cookie *http.Cookie) string {
	p.t.Helper()
	r := rpc(procedure, "")
	r.RemoteAddr = p.client
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return errorCode(p.t, serve(p.h, r))
}

const operatorRPC = lotsmanv1connect.UserServiceListProcedure

// sessionOf is the session cookie w set.
func sessionOf(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no session cookie in %q", w.Header().Values("Set-Cookie"))
	return nil
}

func wantStatus(t *testing.T, what string, w *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	if w.Code != status || !strings.Contains(w.Body.String(), message) {
		t.Errorf("%s: %d %s, want %d %q", what, w.Code, strings.TrimSpace(w.Body.String()), status, message)
	}
}

func TestEnrollSignInSignOut(t *testing.T) {
	p := newAuthPanel(t)
	key := newSoftKey(t, testOrigin, testRPID)
	w := p.enroll(key, p.issue(), "  laptop ")
	enrolled := sessionOf(t, w)
	if !enrolled.HttpOnly || !enrolled.Secure || enrolled.SameSite != http.SameSiteStrictMode || enrolled.Path != "/" ||
		enrolled.Domain != "" || enrolled.MaxAge != int(seconds(sessionMax)) {
		t.Errorf("cookie %s", w.Header().Get("Set-Cookie"))
	}
	if got := p.call(operatorRPC, enrolled); got != "" {
		t.Fatalf("enrolled: code %q", got)
	}
	wantStatus(t, "session", p.post("/auth/session", struct{}{}, enrolled), http.StatusOK, `{"name":"laptop","signedIn":true}`)

	w = p.post("/auth/logout", struct{}{}, enrolled)
	if w.Code != http.StatusNoContent || !strings.Contains(w.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("logout: %d, Set-Cookie %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	if got := p.call(operatorRPC, enrolled); got != "unauthenticated" {
		t.Errorf("after logout: code %q", got)
	}
	wantStatus(t, "session after logout", p.post("/auth/session", struct{}{}, enrolled), http.StatusOK, `{"signedIn":false}`)

	for i := 1; i <= 2; i++ {
		cookie := sessionOf(t, p.login(key))
		if cookie.Value == enrolled.Value {
			t.Error("sign-in reused a session ID")
		}
		if got := p.call(operatorRPC, cookie); got != "" {
			t.Errorf("sign-in %d: code %q", i, got)
		}
		var stored []byte
		var used sql.NullInt64
		p.db.QueryRow(`SELECT credential, used_at FROM passkeys`).Scan(&stored, &used)
		var cred webauthn.Credential
		json.Unmarshal(stored, &cred)
		if cred.Authenticator.SignCount != uint32(i) || used.Int64 != p.now.Unix() {
			t.Errorf("sign-in %d: stored counter %d, used_at %v", i, cred.Authenticator.SignCount, used)
		}
	}
	var hash []byte
	p.db.QueryRow(`SELECT id_hash FROM sessions LIMIT 1`).Scan(&hash)
	if len(hash) != 32 {
		t.Errorf("sessions store %x, want a SHA-256 hash", hash)
	}
}

// Every operator RPC refuses a request without a live session, through
// the real handler chain, and none reaches the daemon.
func TestOperatorRPCsNeedSession(t *testing.T) {
	p := newAuthPanel(t)
	_, cookie := p.signedIn("laptop")
	forged := &http.Cookie{Name: sessionCookie, Value: newSecret()}
	for procedure, scope := range Scopes {
		switch scope {
		case Operator:
			for name, c := range map[string]*http.Cookie{"anonymous": nil, "forged": forged} {
				if got := p.call(procedure, c); got != "unauthenticated" {
					t.Errorf("%s %s: code %q", name, procedure, got)
				}
			}
			if got := p.call(procedure, cookie); got != "" {
				t.Errorf("operator %s: code %q", procedure, got)
			}
		case User:
			if got := p.call(procedure, cookie); got != "unauthenticated" {
				t.Errorf("an operator session is no user link: %s: code %q", procedure, got)
			}
		}
	}
	if n, want := len(p.daemon.calls()), countScope(Operator); n != want {
		t.Errorf("daemon calls = %d, want only the signed-in ones, %d", n, want)
	}
}

func countScope(s Scope) int {
	n := 0
	for _, scope := range Scopes {
		if scope == s {
			n++
		}
	}
	return n
}

func TestEnrollmentLinks(t *testing.T) {
	p := newAuthPanel(t)
	key := newSoftKey(t, testOrigin, testRPID)

	wantStatus(t, "unknown", p.enroll(key, newSecret(), "laptop"), http.StatusNotFound, "unknown enrollment link")

	expired := p.issue()
	p.advance(EnrollmentTTL)
	wantStatus(t, "expired", p.enroll(key, expired, "laptop"), http.StatusGone, "expired")

	used := p.issue()
	sessionOf(t, p.enroll(key, used, "laptop"))
	wantStatus(t, "used", p.post("/auth/enroll/begin", map[string]string{"token": used, "name": "phone"}, nil),
		http.StatusGone, "has been used")

	// Two ceremonies begun with one link: only the first to finish gets in.
	token := p.issue()
	a, b := newSoftKey(t, testOrigin, testRPID), newSoftKey(t, testOrigin, testRPID)
	beginA := p.post("/auth/enroll/begin", map[string]string{"token": token, "name": "a"}, nil)
	beginB := p.post("/auth/enroll/begin", map[string]string{"token": token, "name": "b"}, nil)
	sessionOf(t, p.post("/auth/enroll/finish", a.create(t, beginA.Body.Bytes()), nil))
	wantStatus(t, "second finish", p.post("/auth/enroll/finish", b.create(t, beginB.Body.Bytes()), nil), http.StatusGone, "used")

	// A link that expires while the passkey prompt is open.
	late := p.issue()
	p.advance(EnrollmentTTL - time.Minute)
	begin := p.post("/auth/enroll/begin", map[string]string{"token": late, "name": "late"}, nil)
	p.advance(2 * time.Minute)
	wantStatus(t, "expired mid-ceremony", p.post("/auth/enroll/finish", key.create(t, begin.Body.Bytes()), nil), http.StatusGone, "used or has expired")

	for _, name := range []string{"", "   ", strings.Repeat("x", maxNameRunes+1), "a\nb"} {
		wantStatus(t, "name "+name, p.enroll(key, p.issue(), name), http.StatusBadRequest, "")
	}
	var hash []byte
	p.db.QueryRow(`SELECT token_hash FROM enrollments LIMIT 1`).Scan(&hash)
	if len(hash) != 32 {
		t.Errorf("enrollments store %x, want a SHA-256 hash", hash)
	}
	var n int
	p.db.QueryRow(`SELECT count(*) FROM passkeys`).Scan(&n)
	if n != 2 {
		t.Errorf("passkeys = %d, want 2", n)
	}
}

// A response is accepted only from the panel's origin and RP ID, once,
// within its time, for the ceremony it was asked for.
func TestCeremonyChecks(t *testing.T) {
	p := newAuthPanel(t)
	key, _ := p.signedIn("laptop")
	signIn := func(name string, k *softKey, status int) {
		t.Helper()
		wantStatus(t, name, p.login(k), status, "")
	}

	phish := *key
	phish.origin = "https://vpn.example.com.evil.example"
	signIn("wrong origin", &phish, http.StatusUnauthorized)
	plain := *key
	plain.origin = "http://vpn.example.com"
	signIn("plain http origin", &plain, http.StatusUnauthorized)
	otherRP := *key
	otherRP.rpID = "evil.example"
	signIn("wrong RP ID", &otherRP, http.StatusUnauthorized)
	signIn("unknown passkey", newSoftKey(t, testOrigin, testRPID), http.StatusUnauthorized)
	unverified := *key
	unverified.skipUV = true
	signIn("without user verification", &unverified, http.StatusUnauthorized)
	noUV := newSoftKey(t, testOrigin, testRPID)
	noUV.skipUV = true
	wantStatus(t, "enroll without user verification", p.enroll(noUV, p.issue(), "x"), http.StatusBadRequest, "not accepted")
	wantStatus(t, "enroll from another origin", p.enroll(newSoftKey(t, "https://evil.example", testRPID), p.issue(), "x"),
		http.StatusBadRequest, "not accepted")
	wantStatus(t, "enroll for another RP ID", p.enroll(newSoftKey(t, testOrigin, "evil.example"), p.issue(), "x"),
		http.StatusBadRequest, "not accepted")

	begin := p.post("/auth/login/begin", struct{}{}, nil)
	response := key.get(t, begin.Body.Bytes())
	sessionOf(t, p.post("/auth/login/finish", response, nil))
	wantStatus(t, "replayed", p.post("/auth/login/finish", response, nil), http.StatusBadRequest, "unknown or expired")

	begin = p.post("/auth/login/begin", struct{}{}, nil)
	p.advance(ceremonyTTL)
	wantStatus(t, "expired challenge", p.post("/auth/login/finish", key.get(t, begin.Body.Bytes()), nil),
		http.StatusBadRequest, "unknown or expired")

	// A sign-in challenge does not finish an enrollment.
	begin = p.post("/auth/login/begin", struct{}{}, nil)
	var o struct{ PublicKey struct{ Challenge string } }
	json.Unmarshal(begin.Body.Bytes(), &o)
	options, _ := json.Marshal(map[string]any{"publicKey": map[string]any{
		"challenge": o.PublicKey.Challenge, "user": map[string]string{"id": b64.EncodeToString(randomBytes(32))},
	}})
	wantStatus(t, "sign-in challenge for enrollment", p.post("/auth/enroll/finish", newSoftKey(t, testOrigin, testRPID).create(t, options), nil),
		http.StatusBadRequest, "unknown or expired")

	// A counter that goes back: a cloned key.
	clone := *key
	clone.count = 0
	signIn("cloned", &clone, http.StatusUnauthorized)

	wantStatus(t, "garbage", p.post("/auth/login/finish", []byte(`{"id":"x"}`), nil), http.StatusBadRequest, "malformed")
	signIn("the real one still works", key, http.StatusOK)
}

func TestRevokePasskey(t *testing.T) {
	p := newAuthPanel(t)
	_, cookieA := p.signedIn("a")
	keyB, cookieB := p.signedIn("b")

	w := p.post("/auth/passkeys", struct{}{}, cookieA)
	var list struct {
		Passkeys []struct {
			ID      int64
			Name    string
			UsedAt  *time.Time
			Current bool
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Passkeys) != 2 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	a, b := list.Passkeys[0], list.Passkeys[1]
	if a.Name != "a" || !a.Current || b.Name != "b" || b.Current || b.UsedAt != nil {
		t.Errorf("list = %+v", list.Passkeys)
	}

	wantStatus(t, "revoke b", p.post("/auth/passkeys/revoke", map[string]int64{"id": b.ID}, cookieA), http.StatusOK, `"signedOut":false`)
	if got := p.call(operatorRPC, cookieB); got != "unauthenticated" {
		t.Errorf("b's session after revoke: code %q", got)
	}
	wantStatus(t, "b signs in", p.login(keyB), http.StatusUnauthorized, "not accepted")
	wantStatus(t, "revoke b again", p.post("/auth/passkeys/revoke", map[string]int64{"id": b.ID}, cookieA), http.StatusNotFound, "")

	w = p.post("/auth/passkeys/revoke", map[string]int64{"id": a.ID}, cookieA)
	wantStatus(t, "revoke self", w, http.StatusOK, `"signedOut":true`)
	if !strings.Contains(w.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("revoking one's own passkey keeps the cookie: %q", w.Header().Get("Set-Cookie"))
	}
	if got := p.call(operatorRPC, cookieA); got != "unauthenticated" {
		t.Errorf("a's session after revoke: code %q", got)
	}
	wantStatus(t, "list signed out", p.post("/auth/passkeys", struct{}{}, cookieA), http.StatusUnauthorized, "")
}

func TestSessionIdleExpiry(t *testing.T) {
	p := newAuthPanel(t)
	_, cookie := p.signedIn("laptop")
	for range 3 {
		p.advance(sessionIdle - time.Minute)
		if got := p.call(operatorRPC, cookie); got != "" {
			t.Fatalf("used within the idle time: code %q", got)
		}
	}
	p.advance(sessionIdle)
	if got := p.call(operatorRPC, cookie); got != "unauthenticated" {
		t.Errorf("idle: code %q", got)
	}
	var n int
	p.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n)
	if n != 0 {
		t.Errorf("sessions left: %d", n)
	}
}

func TestSessionAbsoluteExpiry(t *testing.T) {
	p := newAuthPanel(t)
	_, cookie := p.signedIn("laptop")
	step := sessionIdle - 10*time.Minute
	elapsed := time.Duration(0)
	for ; elapsed+step < sessionMax; elapsed += step {
		p.advance(step)
		if got := p.call(operatorRPC, cookie); got != "" {
			t.Fatalf("after %s: code %q", elapsed+step, got)
		}
	}
	p.advance(step)
	if got := p.call(operatorRPC, cookie); got != "unauthenticated" {
		t.Errorf("after %s, in use all along: code %q", elapsed+step, got)
	}
}

func TestSignInRateLimit(t *testing.T) {
	p := newAuthPanel(t)
	for i := range signInBurst {
		if w := p.post("/auth/login/begin", struct{}{}, nil); w.Code != http.StatusOK {
			t.Fatalf("attempt %d: %d", i+1, w.Code)
		}
	}
	w := p.post("/auth/login/begin", struct{}{}, nil)
	wantStatus(t, "over the burst", w, http.StatusTooManyRequests, "too many")
	if w.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	wantStatus(t, "enrollment shares the limit", p.post("/auth/enroll/begin", map[string]string{"token": "x", "name": "x"}, nil),
		http.StatusTooManyRequests, "")

	p.client = "[2001:db8::1]:1234"
	wantStatus(t, "another client", p.post("/auth/login/begin", struct{}{}, nil), http.StatusOK, "")
	p.client = "192.0.2.1:1234"
	p.advance(signInEvery)
	wantStatus(t, "refilled", p.post("/auth/login/begin", struct{}{}, nil), http.StatusOK, "")
	wantStatus(t, "one refilled", p.post("/auth/login/begin", struct{}{}, nil), http.StatusTooManyRequests, "")
}

func TestLimiter(t *testing.T) {
	now := time.Now()
	l := newLimiter(2, time.Minute)
	addr := netip.MustParseAddr
	for _, a := range []string{"2001:db8::1", "2001:db8::2"} {
		if !l.allow(addr(a), now) {
			t.Errorf("%s refused", a)
		}
	}
	if l.allow(addr("2001:db8::ffff"), now) {
		t.Error("an IPv6 /64 counts as one client")
	}
	if !l.allow(addr("2001:db8:0:1::1"), now) || !l.allow(addr("192.0.2.1"), now) || !l.allow(addr("192.0.2.2"), now) {
		t.Error("separate clients share a bucket")
	}
	if l.allow(addr("2001:db8::1"), now.Add(59*time.Second)) || !l.allow(addr("2001:db8::1"), now.Add(time.Minute)) {
		t.Error("refill")
	}

	// The memory is bounded: full buckets go first, then everything.
	l = newLimiter(1, time.Minute)
	l.max = 2
	l.allow(addr("192.0.2.1"), now)
	l.allow(addr("192.0.2.2"), now.Add(time.Minute))
	l.allow(addr("192.0.2.3"), now.Add(time.Minute)) // 192.0.2.1 is full again
	if _, ok := l.clients[clientPrefix(addr("192.0.2.1"))]; ok || len(l.clients) != 2 {
		t.Errorf("clients after pruning: %v", l.clients)
	}
	l.allow(addr("192.0.2.4"), now.Add(time.Minute))
	if len(l.clients) != 1 {
		t.Errorf("clients after a flood: %v", l.clients)
	}
}

func TestCeremoniesBounded(t *testing.T) {
	now := time.Now()
	cs := newCeremonies(2)
	put := func(challenge string) bool {
		return cs.put(ceremony{session: webauthn.SessionData{Challenge: challenge}, expires: now.Add(time.Minute)}, now)
	}
	if !put("a") || !put("b") || put("c") {
		t.Fatal("bound")
	}
	if _, ok := cs.take("a", signIn, now); !ok {
		t.Fatal("take")
	}
	if !put("c") {
		t.Error("taking frees a place")
	}
	now = now.Add(time.Minute)
	if !put("d") {
		t.Error("expired ceremonies free their place")
	}
}

// The sign-in endpoints take only JSON POSTs from the panel's own page.
func TestAuthRefusesForgedRequests(t *testing.T) {
	p := newAuthPanel(t)
	_, cookie := p.signedIn("laptop")
	for _, path := range []string{"/auth/session", "/auth/logout", "/auth/login/begin", "/auth/login/finish",
		"/auth/enroll/begin", "/auth/enroll/finish", "/auth/passkeys", "/auth/passkeys/revoke", "/auth/enrollments"} {
		for _, tc := range []struct {
			name, method, origin, contentType string
			want                              int
		}{
			{"no origin", http.MethodPost, "", "application/json", http.StatusForbidden},
			{"another site", http.MethodPost, "https://evil.example", "application/json", http.StatusForbidden},
			{"form", http.MethodPost, testOrigin, "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
			{"text", http.MethodPost, testOrigin, "text/plain", http.StatusUnsupportedMediaType},
			{"GET", http.MethodGet, testOrigin, "", http.StatusMethodNotAllowed},
			{"GET without origin", http.MethodGet, "", "", http.StatusForbidden},
		} {
			r := httptest.NewRequest(tc.method, path, strings.NewReader(`{}`))
			r.AddCookie(cookie)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			if w := serve(p.h, r); w.Code != tc.want {
				t.Errorf("%s %s: %d, want %d", path, tc.name, w.Code, tc.want)
			}
		}
	}
	if w := p.post("/auth/nope", struct{}{}, cookie); w.Code != http.StatusNotFound {
		t.Errorf("unknown endpoint: %d", w.Code)
	}
	if w := p.post("/auth/session", struct{}{}, cookie); w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", w.Header().Get("Cache-Control"))
	}
}

// An operator issues a link from the panel; it enrolls another passkey.
func TestIssueEnrollmentFromPanel(t *testing.T) {
	p := newAuthPanel(t)
	_, cookie := p.signedIn("laptop")
	wantStatus(t, "anonymous", p.post("/auth/enrollments", struct{}{}, nil), http.StatusUnauthorized, "")
	w := p.post("/auth/enrollments", struct{}{}, cookie)
	var link struct {
		URL       string
		ExpiresAt time.Time
	}
	if err := json.Unmarshal(w.Body.Bytes(), &link); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	token, ok := strings.CutPrefix(link.URL, testOrigin+"/enroll#")
	if !ok || !link.ExpiresAt.Equal(p.now.Add(EnrollmentTTL)) {
		t.Errorf("link = %+v", link)
	}
	sessionOf(t, p.enroll(newSoftKey(t, testOrigin, testRPID), token, "phone"))
}
