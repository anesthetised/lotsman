package web

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"testing/fstest"

	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
)

func TestSecurityHeaders(t *testing.T) {
	h := testPanel(t, fakeAuth{}, &fakeDaemon{})
	refused := rpc(lotsmanv1connect.ProfileServiceListProcedure, "")
	refused.Header.Del("Origin")
	for name, r := range map[string]*http.Request{
		"page":           httptest.NewRequest(http.MethodGet, "/", nil),
		"missing asset":  httptest.NewRequest(http.MethodGet, "/assets/gone.js", nil),
		"api error":      rpc(lotsmanv1connect.ProfileServiceListProcedure, ""),
		"refused origin": refused,
	} {
		w := serve(h, r)
		for header, want := range map[string]string{
			"Content-Security-Policy": "default-src 'self'; base-uri 'none'; object-src 'none'; form-action 'self'; frame-ancestors 'none'",
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "no-referrer",
			"X-Content-Type-Options":  "nosniff",
		} {
			if got := w.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", name, header, got, want)
			}
		}
	}
	if got := serve(h, rpc(lotsmanv1connect.ProfileServiceListProcedure, "")).Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("api Cache-Control = %q, want no-store", got)
	}
}

func TestOriginCheck(t *testing.T) {
	daemon := &fakeDaemon{}
	h := testPanel(t, fakeAuth{operator: true}, daemon)
	for _, tc := range []struct {
		name, method, path, origin string
		want                       int
	}{
		{"api from the panel", http.MethodPost, "/api" + lotsmanv1connect.ProfileServiceListProcedure, testOrigin, http.StatusOK},
		{"api without origin", http.MethodPost, "/api" + lotsmanv1connect.ProfileServiceListProcedure, "", http.StatusForbidden},
		{"api from another site", http.MethodPost, "/api" + lotsmanv1connect.ProfileServiceListProcedure, "https://evil.example", http.StatusForbidden},
		{"api by scheme", http.MethodPost, "/api" + lotsmanv1connect.ProfileServiceListProcedure, "http://vpn.example.com", http.StatusForbidden},
		{"api by port", http.MethodPost, "/api" + lotsmanv1connect.ProfileServiceListProcedure, testOrigin + ":8443", http.StatusForbidden},
		{"api null origin", http.MethodPost, "/api" + lotsmanv1connect.ProfileServiceListProcedure, "null", http.StatusForbidden},
		{"api GET", http.MethodGet, "/api" + lotsmanv1connect.ProfileServiceListProcedure, "", http.StatusForbidden},
		{"page load", http.MethodGet, "/", "", http.StatusOK},
		{"form post elsewhere", http.MethodPost, "/login", "https://evil.example", http.StatusForbidden},
		{"post without origin", http.MethodPost, "/login", "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rpc("", "")
			r.Method, r.URL.Path = tc.method, tc.path
			r.Header.Del("Origin")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			// Host plays no part: a rebound or spoofed one changes nothing.
			r.Host = "vpn.example.com"
			if w := serve(h, r); w.Code != tc.want {
				t.Errorf("status %d, want %d", w.Code, tc.want)
			}
		})
	}
	if n := len(daemon.calls()); n != 1 {
		t.Errorf("daemon calls = %d, want only the panel's own", n)
	}
}

func TestClientAddr(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("10.0.0.0/8")}
	for _, tc := range []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"direct", "198.51.100.1:1234", nil, "198.51.100.1"},
		{"untrusted peer forges", "198.51.100.1:1234", []string{"203.0.113.7"}, "198.51.100.1"},
		{"through the proxy", "127.0.0.1:1234", []string{"203.0.113.7"}, "203.0.113.7"},
		{"client forges a hop", "127.0.0.1:1234", []string{"192.0.2.66, 203.0.113.7"}, "203.0.113.7"},
		{"two trusted proxies", "127.0.0.1:1234", []string{"203.0.113.7, 10.1.1.1"}, "203.0.113.7"},
		{"several headers", "127.0.0.1:1234", []string{"192.0.2.66", "203.0.113.7"}, "203.0.113.7"},
		{"proxy without the header", "127.0.0.1:1234", nil, "127.0.0.1"},
		{"garbage", "127.0.0.1:1234", []string{"203.0.113.7, junk"}, "127.0.0.1"},
		{"ipv4-mapped", "[::ffff:127.0.0.1]:1234", []string{"::ffff:203.0.113.7"}, "203.0.113.7"},
		{"all trusted", "127.0.0.1:1234", []string{"10.2.2.2"}, "10.2.2.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientAddr(r, trusted); got.String() != tc.want {
				t.Errorf("client = %s, want %s", got, tc.want)
			}
		})
	}
}

// The authenticator sees the client's address, for rate limits.
func TestClientAddrReachesAuthenticator(t *testing.T) {
	var seen netip.Addr
	auth := authFunc(func(r *http.Request) error { seen = ClientAddr(r); return nil })
	h := New(Deps{Config: testConfig(t, "trusted_proxies: [127.0.0.1]\n"), Auth: auth, Daemon: &fakeDaemon{}, UI: fstest.MapFS{}, Log: discardLog()})
	r := rpc(lotsmanv1connect.ProfileServiceListProcedure, "")
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	serve(h, r)
	if seen.String() != "203.0.113.7" {
		t.Errorf("authenticator saw %s", seen)
	}
}

type authFunc func(*http.Request) error

func (f authFunc) Operator(r *http.Request) error       { return f(r) }
func (f authFunc) User(r *http.Request) (string, error) { return "", f(r) }
