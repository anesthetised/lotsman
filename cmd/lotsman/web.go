package main

import (
	"context"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/anesthetised/lotsman/internal/web"
)

// newAuthenticator decides who may call operator and user RPCs. Operator
// passkeys (#133) and user links (#134) replace NoAuth; integration tests
// replace it in their own binary only.
var newAuthenticator = func() web.Authenticator { return web.NoAuth{} }

func defaultWebConfigPath() string {
	if p := os.Getenv("LOTSMAN_WEB_CONFIG"); p != "" {
		return p
	}
	return "/etc/lotsman/web.yaml"
}

// runWeb runs the web panel until ctx is done or a signal stops it.
func runWeb(ctx context.Context, configPath string, level slog.Level) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	cfg, err := web.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		log.Warn("the web panel runs as root; run it as lotsman-web, see the operations guide")
	}
	db, err := web.OpenDB(filepath.Join(cfg.StateDir, "web.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	if _, err := fs.Stat(web.Dist(), "index.html"); err != nil {
		log.Warn("this binary has no web UI built in and serves a placeholder page; build it with just web-build")
	}
	h := web.New(web.Deps{
		Config: cfg, Auth: newAuthenticator(), Daemon: web.DaemonTransport(cfg.Socket), UI: web.Dist(), Log: log,
	})
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("web panel serving", "listen", ln.Addr().String(), "public_url", cfg.Origin(), "socket", cfg.Socket)
	return web.Serve(ctx, ln, h)
}
