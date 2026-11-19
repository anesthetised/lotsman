package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/control"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/dataplane/linux"
	"github.com/anesthetised/lotsman/internal/flows"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/metrics"
	"github.com/anesthetised/lotsman/internal/proxy/helper"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

// serve runs the daemon. level filters everything logged, including the AWG
// device logs that tunnel maps to Debug.
func serve(ctx context.Context, configPath string, level slog.Level) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	limitMemory(log)
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	// A second serve must fail before it changes anything the running one
	// owns: its pid file, identity, database (a newer binary would migrate
	// it), interfaces, rules or firewall.
	unlock, err := lockInstance(cfg.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	e, err := openState(cfg)
	if err != nil {
		return err
	}
	defer e.store.Close()

	// Every proxy upstream runs in its own helper process, so a panic in a
	// proxy stack takes down that upstream only.
	helpers := &helper.Runner{Command: helper.Self(log)}
	d := daemon.New(daemon.Deps{
		Version: version, Config: e.cfg, Identity: e.id, Store: e.store,
		Dataplane: linux.New(), NewTUN: tunnel.CreateTUN, Probe: health.ProbeOnInterface, ProbeDNS: health.DNSOnInterface,
		Transfer: health.TransferOnInterface, Proxy: helpers.Start, Log: log,
	})
	// Subscribe before the pid file appears: SIGUSR1 kills a process that has
	// not subscribed yet, and `user add` signals as soon as it finds the file.
	hup, usr1 := make(chan os.Signal, 1), make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(usr1, syscall.SIGUSR1)
	defer signal.Stop(hup)
	defer signal.Stop(usr1)
	if err := writePID(e.cfg.StateDir); err != nil {
		return err
	}
	defer os.Remove(pidPath(e.cfg.StateDir))

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go handleSignals(ctx, d, configPath, hup, usr1, log)
	go enableAccounting(ctx, d, log)
	// serve returns only after the publisher has removed the status file, so
	// the locks are not released while it can still delete a successor's.
	published := make(chan struct{})
	go func() {
		defer close(published)
		publishStatus(ctx, d, e.cfg.StateDir, log)
	}()
	defer func() {
		stop()
		<-published
	}()
	// Created only now that the locks are held, so a refused second serve
	// leaves the running one's socket alone. Like the status file, it is
	// removed before the locks are released.
	ln, err := control.Listen(control.SocketPath, control.Group, log)
	if err != nil {
		return err
	}
	controlled := make(chan struct{})
	go func() {
		defer close(controlled)
		if err := control.Serve(ctx, ln, d); err != nil {
			log.Error("control socket", "err", err)
		}
	}()
	defer func() {
		stop()
		<-controlled
	}()
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

// limitMemory gives the Go runtime a soft memory limit when $GOMEMLIMIT is
// unset, so the daemon's heap does not follow the traffic up to the host's
// RAM. Helper processes do not call it. Failing to read the host's memory
// only means no limit.
func limitMemory(log *slog.Logger) {
	limit, ok := readMemoryLimit(os.Getenv("GOMEMLIMIT"), "/proc", "/sys/fs/cgroup")
	if !ok {
		log.Debug("no default memory limit")
		return
	}
	debug.SetMemoryLimit(limit)
	log.Info("memory limit set", "limit_mib", limit>>20, "percent_of_memory", memoryLimitPercent)
}

// handleSignals serves the two control signals. SIGHUP re-reads the config
// and applies it live; a broken config is logged and ignored, but the user
// database is still reconciled. SIGUSR1, sent by the user and profile
// commands, only reconciles, so a half-edited config file is never applied by
// accident.
func handleSignals(ctx context.Context, d *daemon.Daemon, configPath string, hup, usr1 <-chan os.Signal, log *slog.Logger) {
	// A signal during startup waits in its channel: Reload and Reconcile need
	// a started daemon.
	select {
	case <-ctx.Done():
		return
	case <-d.Ready():
	}
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
		case <-usr1:
			if err := d.Reconcile(); err != nil {
				log.Error("reconcile after SIGUSR1", "err", err)
			}
		}
	}
}

// enableAccounting turns on conntrack's byte counters, which live connections
// need, once the daemon's firewall has loaded nf_conntrack. The gateway works
// without them; connections then show no bytes.
func enableAccounting(ctx context.Context, d *daemon.Daemon, log *slog.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-d.Ready():
	}
	if err := flows.EnableAccounting(); err != nil {
		log.Warn("live connections will show no traffic", "err", err)
	}
}

// statusSource is what publishStatus reads: the daemon, or a fake in tests.
type statusSource interface {
	Ready() <-chan struct{}
	Rounds() <-chan struct{}
	Status() daemon.Status
	ClientMTU() int
}

// publishStatus writes the status file as soon as the daemon is ready and
// then after every health round. It follows the daemon's rounds instead of
// keeping a ticker, so a reload that changes health.interval changes the
// cadence at once.
func publishStatus(ctx context.Context, d statusSource, stateDir string, log *slog.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-d.Ready():
	}
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
		case <-d.Rounds():
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
