package web

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
)

// csp allows the panel's own scripts, styles and API, and nothing inline or
// from elsewhere: the user page's link is a secret.
const csp = "default-src 'self'; base-uri 'none'; object-src 'none'; form-action 'self'; frame-ancestors 'none'"

// secure sets the security headers on every response.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// sameOrigin refuses API and sign-in calls and every unsafe method unless
// the request carries Origin equal to the public URL's: another site cannot
// call them with the operator's cookies, and DNS rebinding gets nowhere.
// Browsers send no Origin on plain page loads, which are GETs of public
// files.
func sameOrigin(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead
		if (!safe || isAPI(r.URL.Path)) && r.Header.Get("Origin") != origin {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isAPI(path string) bool {
	for _, prefix := range []string{apiPrefix, authPrefix} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

type clientKey struct{}

// withClient records the client's address: the peer's own, or, when the
// peer is a trusted proxy, the last address in X-Forwarded-For that is not
// a trusted proxy. Rate limits and logs use it.
func withClient(trusted []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), clientKey{}, clientAddr(r, trusted))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ClientAddr is the address of the client that sent r.
func ClientAddr(r *http.Request) netip.Addr {
	a, _ := r.Context().Value(clientKey{}).(netip.Addr)
	return a
}

func clientAddr(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	addr := peer.Addr().Unmap()
	if !isTrusted(addr, trusted) {
		return addr
	}
	// Each proxy appends the address it got the request from, so walk back
	// from the end until the first hop no trusted proxy vouches for.
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return addr // garbage: trust nothing before it
		}
		addr = hop.Unmap()
		if !isTrusted(addr, trusted) {
			return addr
		}
	}
	return addr
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
