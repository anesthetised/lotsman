// Package web is the web panel, `lotsman web`: it serves the embedded UI and
// forwards the browser's lotsman.v1 calls to the daemon's control socket,
// allowing each RPC only for its scope. It runs unprivileged behind the
// operator's reverse proxy and never opens the daemon's database.
package web

import (
	"context"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Deps is what the panel is built from; tests pass fakes.
type Deps struct {
	Config *Config
	Auth   Authenticator
	// SignIn serves the sign-in endpoints under /auth/, normally the
	// OperatorAuth that is also Auth.
	SignIn http.Handler
	// Daemon carries forwarded calls, normally DaemonTransport.
	Daemon http.RoundTripper
	// OutlineUsage serves cached quota data to operators.
	OutlineUsage http.Handler
	// UI is the frontend's files, normally Dist.
	UI  fs.FS
	Log *slog.Logger
}

// New is the panel's HTTP handler.
func New(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(apiPrefix+"/", newAPI(Scopes, d.Auth, d.Daemon, d.Log))
	mux.HandleFunc("POST "+outlineUsagePath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if err := d.Auth.Operator(r); err != nil {
			fail(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if d.OutlineUsage == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		d.OutlineUsage.ServeHTTP(w, r)
	})
	if d.SignIn != nil {
		mux.Handle(authPrefix+"/", d.SignIn)
	}
	mux.Handle("/", newUI(d.UI))
	return secure(withClient(d.Config.trusted, sameOrigin(d.Config.origin, mux)))
}

// Serve answers on ln until ctx is done, then closes ln. Every request's
// context derives from ctx, so open streams end with it.
func Serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{
		Handler: h,
		// No ReadTimeout or WriteTimeout: they would cut Watch streams. The
		// API bounds reading a body and the UI writing a file instead.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if err != nil {
		srv.Close() // a stream that ignores its context
	}
	<-served // http.ErrServerClosed once Shutdown has begun
	return err
}
