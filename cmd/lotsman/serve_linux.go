package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/dataplane/linux"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/metrics"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

func serve(ctx context.Context, configPath string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()

	d := daemon.New(daemon.Deps{
		Version: version, Config: e.cfg, Identity: e.id, Store: e.store,
		Dataplane: linux.New(), NewTUN: tunnel.CreateTUN, Probe: health.Probe, Log: log,
	})
	if err := writePID(e.cfg.StateDir); err != nil {
		return err
	}
	defer os.Remove(pidPath(e.cfg.StateDir))

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go handleHUP(ctx, d, configPath, log)
	go publishStatus(ctx, d, e.cfg.StateDir, e.cfg.Health.Interval, log)
	if e.cfg.Metrics.Listen != "" {
		go serveMetrics(ctx, e.cfg.Metrics.Listen, metrics.Handler(daemon.Families, d.Metrics), log)
	}

	log.Info("lotsman starting", "listen", e.cfg.Listen, "upstreams", len(e.cfg.Upstreams))
	err = d.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("lotsman stopped")
	return nil
}

// handleHUP re-reads the config on SIGHUP and applies it live; a broken
// config is logged and ignored, but the user database is still reconciled
// so `user add` keeps working.
func handleHUP(ctx context.Context, d *daemon.Daemon, configPath string, log *slog.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			if cfg, err := config.Load(configPath); err != nil {
				log.Error("reload: config not applied", "err", err)
			} else if err := d.Reload(ctx, cfg); err != nil {
				log.Error("reload", "err", err)
			}
			if err := d.Reconcile(); err != nil {
				log.Error("reconcile after SIGHUP", "err", err)
			}
		}
	}
}

// publishStatus writes the status file as soon as the daemon is ready and
// then after every health round.
func publishStatus(ctx context.Context, d *daemon.Daemon, stateDir string, interval time.Duration, log *slog.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-d.Ready():
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		st := d.Status()
		s := statusFile{UpdatedAt: time.Now(), ClientMTU: d.ClientMTU(), Upstreams: st.Upstreams, Peers: st.Peers}
		if err := writeStatus(stateDir, s); err != nil {
			log.Error("write status", "err", err)
		}
		select {
		case <-ctx.Done():
			os.Remove(statusPath(stateDir))
			return
		case <-ticker.C:
		}
	}
}

// serveMetrics answers /metrics on addr. The address is usually the tunnel
// gateway, which exists only once the daemon has brought lm0 up, so binding
// is retried for a while.
func serveMetrics(ctx context.Context, addr string, h http.Handler, log *slog.Logger) {
	var ln net.Listener
	deadline := time.Now().Add(30 * time.Second)
	for {
		var err error
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		if time.Now().After(deadline) {
			log.Error("metrics: cannot listen", "addr", addr, "err", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", h)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Info("metrics listening", "addr", ln.Addr())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("metrics server", "err", err)
	}
}

func writePID(stateDir string) error {
	return os.WriteFile(pidPath(stateDir), []byte(strconv.Itoa(os.Getpid())), 0o644)
}
