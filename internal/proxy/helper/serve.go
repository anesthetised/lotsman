package helper

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"github.com/anesthetised/lotsman/internal/proxy"
)

// Main runs a helper: the Subcommand. It returns the exit code.
func Main() int {
	return run(openTUN)
}

// run serves the control connection on controlFD with the TUN openTUN
// returns, and reports failures on stderr, where the daemon picks them up.
func run(openTUN func() (tun.Device, error)) int {
	// The daemon decides when a helper stops: it closes the control
	// connection, and the kernel kills the helper if the daemon dies. A
	// signal for the whole process group, such as Ctrl-C or systemd
	// stopping the service, must not kill it first: that would look like
	// a crash.
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	if err := harden(); err != nil {
		fmt.Fprintln(os.Stderr, "proxy helper:", err)
		return 1
	}
	f := os.NewFile(controlFD, "control")
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "proxy helper: control connection (only lotsman serve starts helpers):", err)
		return 1
	}
	defer conn.Close()
	if err := Serve(conn, openTUN, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "proxy helper:", err)
		return 1
	}
	return 0
}

// Serve is the helper's side of the control connection. It waits for open,
// starts the stack on the TUN openTUN returns, logging JSON records to
// logOut, and answers requests until the daemon closes conn; then it stops
// the stack and returns nil. If the stack stops on its own, because reading
// the TUN failed, Serve closes conn and returns that error, so the helper
// exits and the daemon restarts it.
func Serve(conn net.Conn, openTUN func() (tun.Device, error), logOut io.Writer) error {
	var open request
	if err := readFrame(conn, &open); err != nil {
		return fmt.Errorf("read open: %w", err)
	}
	failed := make(chan error, 1)
	link, err := start(open, openTUN, logOut, func(err error) {
		failed <- err
		conn.Close()
	})
	if err != nil {
		// The daemon reports it; the reply is best effort.
		writeFrame(conn, response{Error: err.Error()})
		return err
	}
	defer link.Close()
	err = writeFrame(conn, response{})
	for err == nil {
		var req request
		if err = readFrame(conn, &req); err == nil {
			err = writeFrame(conn, handle(link, req))
		}
	}
	select {
	case err := <-failed:
		return err
	default:
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func start(open request, openTUN func() (tun.Device, error), logOut io.Writer, failed func(error)) (proxy.Link, error) {
	if open.Op != opOpen || open.Spec == nil {
		return nil, fmt.Errorf("first request is %q, want %s", open.Op, opOpen)
	}
	t, err := openTUN()
	if err != nil {
		return nil, fmt.Errorf("tun: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: open.LogLevel}))
	return proxy.InProcess(t, *open.Spec, log, failed)
}

func handle(link proxy.Link, req request) response {
	var resp response
	var err error
	switch req.Op {
	case opServer:
		err = link.SetServer(req.Key, req.Server)
	case opMaxFlows:
		err = link.SetMaxFlows(req.MaxFlows)
	case opStats:
		var st proxy.Stats
		st, err = link.Stats()
		resp.Stats = &st
	default:
		err = fmt.Errorf("unknown request %q", req.Op)
	}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}
