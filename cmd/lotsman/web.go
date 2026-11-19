package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/anesthetised/lotsman/internal/web"
)

// newAuthenticator decides who may call operator and user RPCs: the
// operators' passkey sessions. Integration tests replace it in their own
// binary only.
var newAuthenticator = func(operators *web.OperatorAuth) web.Authenticator { return operators }

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
	operators, err := web.NewOperatorAuth(cfg, db, log)
	if err != nil {
		return err
	}
	h := web.New(web.Deps{
		Config: cfg, Auth: newAuthenticator(operators), SignIn: operators,
		Daemon: web.DaemonTransport(cfg.Socket), UI: web.Dist(), Log: log,
	})
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("web panel serving", "listen", ln.Addr().String(), "public_url", cfg.Origin(), "socket", cfg.Socket)
	return web.Serve(ctx, ln, h)
}

// webEnroll prints a one-time link that enrolls an operator's passkey. It
// writes web.db itself, so it runs on the panel's host as lotsman-web or
// root; the panel need not be running.
func webEnroll(configPath string) error {
	cfg, err := web.LoadConfig(configPath)
	if err != nil {
		return err
	}
	db, err := web.OpenDB(filepath.Join(cfg.StateDir, "web.db"))
	if err != nil {
		return err
	}
	token, expires, err := web.IssueEnrollment(context.Background(), db, time.Now())
	db.Close()
	if err != nil {
		return err
	}
	if err := giveToStateOwner(cfg.StateDir); err != nil {
		return err
	}
	fmt.Printf("Open this link in the browser that will hold the passkey. It works once, until %s:\n%s\n",
		expires.UTC().Format("2006-01-02 15:04 MST"), cfg.EnrollURL(token))
	return nil
}

// giveToStateOwner hands web.db and its journal files, if root created
// them, to the owner of the state directory, the user the panel runs as:
// it could not open root's files.
func giveToStateOwner(dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st := fi.Sys().(*syscall.Stat_t)
	for _, name := range []string{"web.db", "web.db-wal", "web.db-shm"} {
		err := os.Lchown(filepath.Join(dir, name), int(st.Uid), int(st.Gid))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
