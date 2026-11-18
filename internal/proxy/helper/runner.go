package helper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"github.com/anesthetised/lotsman/internal/proxy"
)

// Runner starts proxy links whose stacks run in helper processes. Its
// Start is a proxy.Runner.
type Runner struct {
	// Command returns a new helper command; Start adds the TUN and the
	// control connection as fds 3 and 4, and stderr. In production it is
	// Self(log).
	Command func() *exec.Cmd
	// Zero values take the defaults.
	StartTimeout   time.Duration // for a helper to answer open; default 10 s
	RequestTimeout time.Duration // for any other answer; default 2 s
	StopTimeout    time.Duration // for a helper to exit once told to; default 5 s
	Backoff        time.Duration // the first restart delay after a quick exit; default 1 s
}

const (
	// stableRun is how long a helper must have run for its exit not to
	// count as a quick one: the next start is not held back then.
	stableRun = time.Minute
	// maxBackoff caps the restart delay after repeated quick exits.
	maxBackoff = time.Minute
)

// errBackoff is constant so the daemon logs it once per outage.
var errBackoff = errors.New("proxy helper: restart held back after it exited soon after starting")

func or(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

// Start returns a link that runs spec in a helper on t's descriptor. No
// helper runs until Up. t must be a kernel TUN: its descriptor is what the
// helper gets.
func (r *Runner) Start(t tun.Device, spec proxy.Spec, log *slog.Logger, exited func(error)) (proxy.Link, error) {
	f := t.File()
	if f == nil {
		t.Close()
		return nil, errors.New("proxy helper: the TUN has no descriptor to pass")
	}
	return &link{r: r, tun: t, file: f, spec: spec, log: log, exited: exited}, nil
}

// link runs one upstream's helper, one at a time, on the same TUN.
type link struct {
	r      *Runner
	tun    tun.Device
	file   *os.File // tun's descriptor, inherited by every helper
	log    *slog.Logger
	exited func(error)

	mu        sync.Mutex // held for every request, so they never interleave
	spec      proxy.Spec // the latest, for the next start
	proc      *process   // the running helper, nil when none
	closed    bool
	delay     time.Duration // the current restart delay
	notBefore time.Time     // when the next start may happen
}

// process is one helper.
type process struct {
	cmd     *exec.Cmd
	conn    net.Conn
	started time.Time
	done    chan struct{} // closed once the helper is reaped and its stderr drained
}

func (l *link) Up() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("proxy helper: link closed")
	}
	if l.proc != nil {
		return nil
	}
	if time.Now().Before(l.notBefore) {
		return errBackoff
	}
	p, err := l.start()
	if err != nil {
		return err
	}
	open := request{Op: opOpen, Spec: &l.spec, LogLevel: minLevel(l.log)}
	if _, err := p.call(open, or(l.r.StartTimeout, 10*time.Second)); err != nil {
		p.stop(0)
		return fmt.Errorf("proxy helper: open: %w", err)
	}
	l.proc = p
	l.log.Info("proxy helper started", "pid", p.cmd.Process.Pid)
	return nil
}

// start runs a helper and watches it; callers hold l.mu.
func (l *link) start() (*process, error) {
	parent, child, err := socketPair()
	if err != nil {
		return nil, fmt.Errorf("proxy helper: control connection: %w", err)
	}
	defer child.Close()
	conn, err := net.FileConn(parent)
	parent.Close()
	if err != nil {
		return nil, fmt.Errorf("proxy helper: control connection: %w", err)
	}
	stderr, stderrW, err := os.Pipe()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy helper: %w", err)
	}
	defer stderrW.Close()

	cmd := l.r.Command()
	cmd.ExtraFiles = []*os.File{l.file, child} // fds 3 and 4
	cmd.Stderr = stderrW
	if err := spawn(cmd); err != nil {
		conn.Close()
		stderr.Close()
		return nil, fmt.Errorf("proxy helper: start: %w", err)
	}
	p := &process{cmd: cmd, conn: conn, started: time.Now(), done: make(chan struct{})}
	go l.watch(p, stderr)
	return p, nil
}

// watch relays the helper's stderr, reaps it and, if it exited on its own
// after it was up (Up sets l.proc only then), reports that and holds the
// next start back.
func (l *link) watch(p *process, stderr *os.File) {
	crash := relayLog(stderr, l.log)
	stderr.Close()
	err := p.cmd.Wait()
	close(p.done)

	// Whoever stops a helper closes its connection; one that exited on its
	// own is no longer used once l.proc is cleared, under l.mu like every
	// request.
	l.mu.Lock()
	unexpected := l.proc == p // nobody asked it to stop
	if unexpected {
		l.proc = nil
		p.conn.Close()
		if time.Since(p.started) >= stableRun {
			l.delay = 0
		} else {
			l.delay = min(max(2*l.delay, or(l.r.Backoff, time.Second)), maxBackoff)
		}
		l.notBefore = time.Now().Add(l.delay)
	}
	delay := l.delay
	l.mu.Unlock()
	if !unexpected {
		l.log.Debug("proxy helper stopped", "pid", p.cmd.Process.Pid, "status", exitStatus(err))
		return
	}
	args := []any{"pid", p.cmd.Process.Pid, "status", exitStatus(err), "restart_delay", delay}
	if crash != "" {
		args = append(args, "panic", crash)
	}
	l.log.Error("proxy helper exited", args...)
	if l.exited != nil {
		l.exited(fmt.Errorf("proxy helper exited: %s", exitStatus(err)))
	}
}

func exitStatus(err error) string {
	if err == nil {
		return "exited with status 0"
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.Error() // "exit status 2", "signal: killed"
	}
	return err.Error()
}

// call sends a request and waits up to timeout for the answer. A helper
// that does not answer in time is stuck: it is killed, and watch reports
// the exit as for a crash. Callers hold l.mu.
func (p *process) call(req request, timeout time.Duration) (response, error) {
	var resp response
	p.conn.SetDeadline(time.Now().Add(timeout))
	err := writeFrame(p.conn, req)
	if err == nil {
		err = readFrame(p.conn, &resp)
	}
	if err != nil {
		p.cmd.Process.Kill()
		return resp, err
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// stop closes the control connection, which tells the helper to exit,
// kills it if it has not exited within timeout and waits until it is reaped.
func (p *process) stop(timeout time.Duration) {
	p.conn.Close()
	select {
	case <-p.done:
		return
	case <-time.After(timeout):
	}
	p.cmd.Process.Kill()
	<-p.done
}

// request sends req to the running helper; callers hold l.mu.
func (l *link) request(req request) (response, error) {
	if l.proc == nil {
		return response{}, errors.New("proxy helper is not running")
	}
	resp, err := l.proc.call(req, or(l.r.RequestTimeout, 2*time.Second))
	if err != nil {
		err = fmt.Errorf("proxy helper: %s: %w", req.Op, err)
	}
	return resp, err
}

// SetServer applies to the running helper, if any, and to the next one.
func (l *link) SetServer(key string, addr netip.AddrPort) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.proc != nil {
		// On failure the daemon keeps what runs and tries again later.
		if _, err := l.request(request{Op: opServer, Key: key, Server: addr}); err != nil {
			return err
		}
	}
	l.spec.Key, l.spec.Server = key, addr
	return nil
}

// SetMaxFlows applies to the running helper, if any, and to the next one.
func (l *link) SetMaxFlows(n int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spec.MaxFlows = n
	if l.proc == nil {
		return nil
	}
	_, err := l.request(request{Op: opMaxFlows, MaxFlows: n})
	return err
}

func (l *link) Stats() (proxy.Stats, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	resp, err := l.request(request{Op: opStats})
	if err != nil {
		return proxy.Stats{}, err
	}
	if resp.Stats == nil {
		return proxy.Stats{}, errors.New("proxy helper: stats: empty answer")
	}
	return *resp.Stats, nil
}

// Close stops the helper, waiting until it is reaped, then closes the TUN.
func (l *link) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	p := l.proc
	l.proc = nil
	l.mu.Unlock()
	if p != nil {
		p.stop(or(l.r.StopTimeout, 5*time.Second))
	}
	l.tun.Close()
}

// minLevel is the lowest level log records, so the helper does not send
// what the daemon would drop.
func minLevel(log *slog.Logger) slog.Level {
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn} {
		if log.Enabled(context.Background(), level) {
			return level
		}
	}
	return slog.LevelError
}

// socketPair returns both ends of a connected stream socket pair, not
// inherited by any child unless passed explicitly.
func socketPair() (*os.File, *os.File, error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "control"), os.NewFile(uintptr(fds[1]), "control"), nil
}

// spawner starts every helper from one goroutine locked to a thread that
// never exits: Linux sends a helper its parent-death signal when the thread
// that started it exits, not the process (go.dev/issue/27505).
var spawner = sync.OnceValue(func() chan<- func() {
	c := make(chan func())
	go func() {
		runtime.LockOSThread()
		for f := range c {
			f()
		}
	}()
	return c
})

func spawn(cmd *exec.Cmd) error {
	errc := make(chan error, 1)
	spawner() <- func() { errc <- cmd.Start() }
	return <-errc
}
