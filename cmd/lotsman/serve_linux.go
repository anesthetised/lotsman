package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/dataplane/linux"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/mtu"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

func serve(ctx context.Context, configPath string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()

	var ups []*daemon.Upstream
	for _, u := range e.cfg.Upstreams {
		up, err := daemon.LoadUpstream(ctx, u, e.cfg.Health)
		if err != nil {
			return err
		}
		t, err := tunnel.CreateTUN(u.InterfaceName(), up.MTU)
		if err != nil {
			return fmt.Errorf("create %s: %w (root or CAP_NET_ADMIN required)", u.InterfaceName(), err)
		}
		up.Device = tunnel.New(u.InterfaceName(), t, log)
		ups = append(ups, up)
	}
	downTUN, err := tunnel.CreateTUN(daemon.DownstreamInterface, mtu.DefaultWireGuard)
	if err != nil {
		return fmt.Errorf("create %s: %w", daemon.DownstreamInterface, err)
	}
	down := tunnel.New(daemon.DownstreamInterface, downTUN, log)

	d := daemon.New(e.cfg, e.id, e.store, linux.New(), down, ups, health.Probe, log)
	if err := writePID(e.cfg.StateDir); err != nil {
		return err
	}
	defer os.Remove(pidPath(e.cfg.StateDir))

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go handleHUP(ctx, d, log)
	go publishStatus(ctx, d, e.cfg.StateDir, e.cfg.Health.Interval, log)

	log.Info("lotsman starting", "listen", e.cfg.Listen, "upstreams", len(ups), "client_mtu", d.ClientMTU())
	err = d.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("lotsman stopped")
	return nil
}

func handleHUP(ctx context.Context, d *daemon.Daemon, log *slog.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			if err := d.Reconcile(); err != nil {
				log.Error("reconcile after SIGHUP", "err", err)
			}
		}
	}
}

func publishStatus(ctx context.Context, d *daemon.Daemon, stateDir string, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			os.Remove(statusPath(stateDir))
			return
		case <-ticker.C:
			s := statusFile{UpdatedAt: time.Now(), ClientMTU: d.ClientMTU(), Upstreams: d.Status()}
			if err := writeStatus(stateDir, s); err != nil {
				log.Error("write status", "err", err)
			}
		}
	}
}

func writePID(stateDir string) error {
	return os.WriteFile(pidPath(stateDir), []byte(strconv.Itoa(os.Getpid())), 0o644)
}
