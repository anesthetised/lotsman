package daemon

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/proxy"
	"github.com/anesthetised/lotsman/internal/store"
)

// dynPath is the secret part of the dynamic key URLs in these tests.
const dynPath = "/s3cr3t-dyn-path"

func ssKeyFor(server string) string {
	return "ss://" + base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:"+ssSecret)) + "@" + server + "/?outline=1"
}

// keyServer serves a dynamic key over HTTPS. fetchClient trusts its
// certificate for the test, with verification on.
type keyServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	body   string
	status int
	hits   int
}

func newKeyServer(t *testing.T, body string) *keyServer {
	t.Helper()
	ks := &keyServer{body: body, status: http.StatusOK}
	ks.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ks.mu.Lock()
		defer ks.mu.Unlock()
		ks.hits++
		if r.URL.Path != dynPath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(ks.status)
		fmt.Fprint(w, ks.body)
	}))
	t.Cleanup(ks.srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(ks.srv.Certificate())
	old := fetchClient
	fetchClient = proxy.NewFetchClient(roots)
	t.Cleanup(func() { fetchClient = old })
	return ks
}

func (ks *keyServer) set(body string, status int) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.body, ks.status = body, status
}

func (ks *keyServer) requests() int {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return ks.hits
}

// url is what the operator puts in the key file.
func (ks *keyServer) url() string {
	return strings.Replace(ks.srv.URL, "https://", "ssconf://", 1) + dynPath + "#My%20key"
}

// logBuffer collects the daemon's log for checks.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// noSecrets fails if s shows the key's URL or its password.
func noSecrets(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, "s3cr3t") || strings.Contains(s, ssSecret) {
		t.Errorf("%s shows a secret:\n%s", what, s)
	}
}

// newDynamicHarness is newProxyHarness with ol's key file holding a dynamic
// key's URL; the key server first serves a key for ss.provider.test.
func newDynamicHarness(t *testing.T, status int) (*harness, *keyServer, *logBuffer) {
	t.Helper()
	h, dns := newProxyHarness(t)
	dns.set("ss.other.test", "192.0.2.20")
	ks := newKeyServer(t, ssKeyFor("ss.provider.test:8388"))
	ks.set(ssKeyFor("ss.provider.test:8388"), status)
	if err := os.WriteFile(filepath.Join(h.dir, "ol.key"), []byte(ks.url()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	h.d.log = slog.New(slog.NewTextHandler(logs, nil))
	t.Cleanup(func() { noSecrets(t, "the log", logs.String()) })
	return h, ks, logs
}

// upstream returns the running upstream called name, or nil.
func (h *harness) upstream(name string) *Upstream {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	for _, u := range h.d.ups {
		if u.Name == name {
			return u
		}
	}
	return nil
}

// serving reports whether ol's link dials server with the running key.
func (h *harness) serving(link link, resolved string) bool {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	for _, u := range h.d.ups {
		if u.Name == "ol" {
			l, ok := u.link.(*proxyLink)
			return ok && u.link == link && u.Resolved == resolved && l.key == u.keyText
		}
	}
	return false
}

func TestLoadDynamicUpstream(t *testing.T) {
	h, ks, _ := newDynamicHarness(t, http.StatusOK)
	u, err := LoadUpstream(h.cfg.Upstreams[1], h.cfg.Health)
	if err != nil {
		t.Fatal(err)
	}
	if u.Location == nil || u.Key != nil || u.Endpoint != "" || ks.requests() != 0 {
		t.Fatalf("loading must not fetch: %+v, %d requests", u, ks.requests())
	}
	noSecrets(t, "the location's formatting", fmt.Sprintf("%v %+v", u.Location, u.Location))
	if err := u.Resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "not fetched") {
		t.Errorf("Resolve before Fetch: %v", err)
	}
	if err := u.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := u.Resolve(context.Background()); err != nil || u.Endpoint != "ss.provider.test:8388" || u.Resolved != "192.0.2.10:8388" {
		t.Errorf("after Fetch: %v, endpoint %q resolved %q", err, u.Endpoint, u.Resolved)
	}

	ks.set(`{"error": {"message": "Subscription expired", "details": "`+ssSecret+`"}}`, http.StatusOK)
	err = u.Fetch(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "upstream ol: ") || !strings.Contains(err.Error(), "Subscription expired") {
		t.Errorf("provider error: %v", err)
	}
	noSecrets(t, "the fetch error", err.Error())

	os.WriteFile(filepath.Join(h.dir, "ol.key"), []byte("ssconf://"+dynPath[1:]+":banana/x\n"), 0o600)
	if _, err := LoadUpstream(h.cfg.Upstreams[1], h.cfg.Health); err == nil {
		t.Error("a malformed URL loaded")
	} else {
		noSecrets(t, "the load error", err.Error())
	}
}

func TestDynamicKeyRefresh(t *testing.T) {
	h, ks, logs := newDynamicHarness(t, http.StatusOK)
	h.st.CreateUser("alice")
	peer, _ := h.st.AddPeer("alice", store.DefaultDevice, "px", h.cfg.Subnet)
	h.run(t)
	routedVia := func(want string) func() bool {
		return func() bool { got, _ := h.dp.RouteOf(peer.IP); return got == want }
	}
	eventually(t, "routed via ol", routedVia("lm-up-ol"))
	link := h.upstream("ol").link
	if !h.serving(link, "192.0.2.10:8388") {
		t.Fatal("ol does not run the fetched key")
	}

	// The fetched key is cached privately, under the URL's fingerprint only.
	path := keyCachePath(h.dir, "ol")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache: %v %v", err, info)
	}
	if data := mustRead(t, path); strings.Contains(data, "s3cr3t") || !strings.Contains(data, "ss.provider.test") {
		t.Errorf("cache content: %s", data)
	}

	// Failing probes do not refetch within keyRefreshInterval of the last fetch.
	fetches := ks.requests()
	ks.set(ssKeyFor("ss.other.test:8388"), http.StatusOK)
	h.setHealthy("lm-up-ol", false)
	eventually(t, "failover to nl", routedVia("lm-up-nl"))
	time.Sleep(150 * time.Millisecond) // several health rounds
	if ks.requests() != fetches {
		t.Fatalf("%d fetches inside the refresh interval", ks.requests()-fetches)
	}

	// Once due, the failing upstream fetches again, once, and the rotated key
	// swaps the dialers of the running link.
	h.d.mu.Lock()
	h.d.fetched["ol"] = time.Now().Add(-keyRefreshInterval)
	h.d.mu.Unlock()
	eventually(t, "rotated key running", func() bool { return h.serving(link, "192.0.2.20:8388") })
	time.Sleep(150 * time.Millisecond)
	if ks.requests() != fetches+1 {
		t.Errorf("%d fetches after the interval, want 1", ks.requests()-fetches)
	}
	if !strings.Contains(logs.String(), "upstream key changed") {
		t.Error("key change not logged")
	}
	u := h.upstream("ol")
	if cached, err := loadCachedKey(h.dir, "ol", u.Location); err != nil || cached.key.Server() != "ss.other.test:8388" {
		t.Errorf("cache after rotation: %v, %v", cached.key, err)
	}

	// A failed fetch keeps the running key.
	ks.set(ssSecret, http.StatusServiceUnavailable)
	h.d.mu.Lock()
	h.d.fetched["ol"] = time.Now().Add(-keyRefreshInterval)
	h.d.mu.Unlock()
	eventually(t, "failed fetch", func() bool { return strings.Contains(logs.String(), "keeping the running key") })
	if !h.serving(link, "192.0.2.20:8388") {
		t.Error("a failed fetch changed the running key")
	}
	if !strings.Contains(logs.String(), "HTTP status 503") {
		t.Error("fetch failure not logged")
	}
	h.setHealthy("lm-up-ol", true)
	eventually(t, "back to ol", routedVia("lm-up-ol"))
}

func TestDynamicKeyFromCache(t *testing.T) {
	h, ks, logs := newDynamicHarness(t, http.StatusOK)
	loc, err := proxy.ParseLocation(ks.url())
	if err != nil {
		t.Fatal(err)
	}
	other, _ := proxy.ParseLocation(strings.Replace(ks.url(), dynPath, dynPath+"-old", 1))
	cache := func(l *proxy.Location, server string) {
		t.Helper()
		text := ssKeyFor(server)
		key, err := proxy.ParseConfig(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := saveCachedKey(h.dir, "ol", l, dynamicKey{key, text}); err != nil {
			t.Fatal(err)
		}
	}

	// A key cached for another URL is not used.
	cache(other, "ss.other.test:8388")
	ks.set("", http.StatusForbidden)
	if _, err := h.d.loadResolved(context.Background(), h.cfg.Upstreams[1], h.cfg.Health, h.dir, nil); err == nil || !strings.Contains(err.Error(), "no usable cached key") {
		t.Errorf("load with the cache of another URL: %v", err)
	}

	// The daemon starts on the key cached for this URL while the URL fails.
	cache(loc, "ss.other.test:8388")
	h.st.CreateUser("alice")
	peer, _ := h.st.AddPeer("alice", store.DefaultDevice, "px", h.cfg.Subnet)
	h.run(t)
	eventually(t, "routed via ol", func() bool { got, _ := h.dp.RouteOf(peer.IP); return got == "lm-up-ol" })
	if u := h.upstream("ol"); u.Resolved != "192.0.2.20:8388" {
		t.Errorf("ol runs %q, want the cached key's server", u.Resolved)
	}
	if !strings.Contains(logs.String(), "using the cached key") {
		t.Error("cache fallback not logged")
	}
}

func TestDynamicKeyWithoutFetchOrCache(t *testing.T) {
	h, ks, logs := newDynamicHarness(t, http.StatusNotFound)
	h.st.CreateUser("alice")
	peer, _ := h.st.AddPeer("alice", store.DefaultDevice, "px", h.cfg.Subnet)
	h.run(t) // fails the test if the daemon stops

	// ol stays down and nl carries the profile; ol is retried every round.
	eventually(t, "routed via nl", func() bool { got, _ := h.dp.RouteOf(peer.IP); return got == "lm-up-nl" })
	if h.upstream("ol") != nil {
		t.Fatal("ol runs without a key")
	}
	if !strings.Contains(logs.String(), "upstream is not running, will retry every health round") {
		t.Error("missing key not logged")
	}
	ks.set(ssKeyFor("ss.provider.test:8388"), http.StatusOK)
	eventually(t, "ol once the URL answers", func() bool { got, _ := h.dp.RouteOf(peer.IP); return got == "lm-up-ol" })
	if _, err := os.Stat(keyCachePath(h.dir, "ol")); err != nil {
		t.Errorf("key not cached: %v", err)
	}
}

func TestDynamicKeyReload(t *testing.T) {
	h, ks, _ := newDynamicHarness(t, http.StatusOK)
	h.run(t)
	eventually(t, "start", h.dp.Ready)
	link := h.upstream("ol").link

	// A reload fetches the key again even though the file is unchanged, and
	// a rotated key applies to the running link.
	ks.set(ssKeyFor("ss.other.test:8388"), http.StatusOK)
	fetches := ks.requests()
	if err := h.d.Reload(context.Background(), h.cfg); err != nil {
		t.Fatal(err)
	}
	if ks.requests() != fetches+1 || !h.serving(link, "192.0.2.20:8388") {
		t.Errorf("after reload: %d fetches, serving the new key %v", ks.requests()-fetches, h.serving(link, "192.0.2.20:8388"))
	}

	// A failed fetch is reported and keeps the running key.
	ks.set(ssSecret, http.StatusInternalServerError)
	err := h.d.Reload(context.Background(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "upstream ol: fetch dynamic key: HTTP status 500") {
		t.Errorf("reload with a failing URL: %v", err)
	} else {
		noSecrets(t, "the reload error", err.Error())
	}
	if !h.serving(link, "192.0.2.20:8388") {
		t.Error("a failed fetch replaced the running key")
	}

	// Removing the upstream deletes its cached key.
	without := strings.Replace(fmt.Sprintf(proxyConfig, h.dir, ""), "  - {name: ol, kind: outline, conf: "+h.dir+"/ol.key, geo: OL}\n", "", 1)
	cfg, err := config.Parse([]byte(without))
	if err != nil || len(cfg.Upstreams) != 1 {
		t.Fatalf("config without ol: %v, %d upstreams", err, len(cfg.Upstreams))
	}
	if err := h.d.Reload(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyCachePath(h.dir, "ol")); !os.IsNotExist(err) {
		t.Errorf("cached key after removing ol: %v", err)
	}
}
