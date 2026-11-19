package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/anesthetised/lotsman/internal/control"
)

const (
	// apiPrefix is where the browser's Connect transport points:
	// /api/lotsman.v1.StatusService/Watch is the daemon's
	// /lotsman.v1.StatusService/Watch.
	apiPrefix = "/api"
	// maxRequestBytes bounds a request body. The API has no client streams,
	// and its largest request, a profile, is a few hundred bytes.
	maxRequestBytes = 64 << 10
	// bodyTimeout bounds reading a request body. The server has no
	// ReadTimeout: on HTTP/1 it would also cancel long-running streams.
	bodyTimeout = 10 * time.Second
)

// DaemonTransport carries requests to the daemon's control socket. It never
// compresses on its own, so Connect's negotiated compression passes through
// untouched.
func DaemonTransport(socket string) http.RoundTripper {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		DisableCompression:  true,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     time.Minute,
	}
}

// api forwards lotsman.v1 to the daemon, checking each procedure's scope
// first. It forwards bytes rather than decoding messages: the daemon
// validates everything, and the panel needs no code per RPC.
type api struct {
	scopes map[string]Scope
	auth   Authenticator
	proxy  *httputil.ReverseProxy
	errors *connecthttp.ErrorWriter
	log    *slog.Logger
	// recheck is how often a user's call that is still running, such as a
	// Watch stream, checks that its link still resolves.
	recheck time.Duration
}

func newAPI(scopes map[string]Scope, auth Authenticator, daemon http.RoundTripper, log *slog.Logger) *api {
	a := &api{scopes: scopes, auth: auth, errors: connecthttp.NewErrorWriter(), log: log, recheck: linkTTL}
	a.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "lotsman", ""
		},
		Transport: daemon,
		// Send each stream message on as it arrives.
		FlushInterval: -1,
		ErrorHandler:  a.daemonError,
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return a
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	procedure := strings.TrimPrefix(r.URL.Path, apiPrefix)
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		a.fail(w, r, connect.NewError(connect.CodeUnimplemented, "RPCs are POST"))
		return
	}
	scope, ok := a.scopes[procedure]
	if !ok || scope == Internal {
		a.fail(w, r, connect.NewError(connect.CodePermissionDenied, "not available through the panel"))
		return
	}
	var user string
	var err error
	switch scope {
	case Operator:
		err = a.auth.Operator(r)
	case User:
		if user, err = a.auth.User(r); err == nil && user == "" {
			err = errors.New("no user")
		}
	}
	if err != nil {
		// An authenticator may pick the code, such as for a rate limit.
		if connect.CodeOf(err) == connect.CodeUnknown {
			err = connect.NewError(connect.CodeUnauthenticated, err.Error())
		}
		a.fail(w, r, err)
		return
	}

	// Read the whole body under a deadline: requests are small, and the
	// limit then fails the call itself, not the copy to the daemon.
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(bodyTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	rc.SetReadDeadline(time.Time{})
	if err != nil {
		code := connect.CodeInvalidArgument
		if errors.As(err, new(*http.MaxBytesError)) {
			code = connect.CodeResourceExhausted
		}
		a.fail(w, r, connect.NewError(code, "request body: "+err.Error()))
		return
	}

	ctx := r.Context()
	if user != "" {
		var stop context.CancelFunc
		ctx, stop = a.whileUser(r, user)
		defer stop()
	}
	out := r.Clone(ctx)
	out.URL.Path, out.URL.RawPath = procedure, ""
	out.Header = forwardHeaders(r.Header)
	if user != "" {
		out.Header.Set(control.UserHeader, user)
	}
	out.Body, out.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	a.proxy.ServeHTTP(w, out)
}

// whileUser is r's context, cancelled once r's link no longer resolves to
// user, so a revoked link also ends the streams it opened.
func (a *api) whileUser(r *http.Request, user string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	go func() {
		tick := time.NewTicker(a.recheck)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			if again, err := a.auth.User(r); err != nil || again != user {
				a.log.Debug("user call ended: link no longer valid", "client", ClientAddr(r), "error", err)
				cancel()
				return
			}
		}
	}()
	return ctx, cancel
}

// forwardHeaders keeps only what the RPC protocols need. Everything else a
// browser sends stays with the panel: cookies, credentials, forwarded
// addresses and above all Lotsman-*, which the daemon trusts.
func forwardHeaders(in http.Header) http.Header {
	out := make(http.Header)
	for name, values := range in {
		switch {
		case name == "Content-Type", name == "Content-Encoding", name == "Accept-Encoding",
			name == "Te", name == "X-Grpc-Web",
			strings.HasPrefix(name, "Connect-"), strings.HasPrefix(name, "Grpc-"):
			out[name] = values
		}
	}
	return out
}

// daemonError answers a call the daemon did not take: it is not running,
// or the panel may not use its socket.
func (a *api) daemonError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the browser or the panel went away
	}
	a.log.Warn("daemon unreachable", "procedure", r.URL.Path, "error", err)
	a.fail(w, r, connect.NewError(connect.CodeUnavailable, "the gateway daemon is not reachable"))
}

func (a *api) fail(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Debug("refused", "path", r.URL.Path, "client", ClientAddr(r), "error", err)
	a.errors.Write(w, r, err)
}
