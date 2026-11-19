package control

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	// SocketPath is where the daemon serves the API.
	SocketPath = "/run/lotsman/control.sock"
	// Group is the system group that may connect to the socket: the web
	// panel's user.
	Group = "lotsman-web"
)

// Listen creates the unix socket at path with mode 0660 and the given group.
// The caller must hold the instance locks, so that no other daemon can be
// using it. A socket left by a daemon that died is replaced; one that still
// accepts connections is refused, as it belongs to a daemon in another
// network namespace. Without the group the socket is root-only (0600) and a
// warning is logged: the gateway does not need the panel to run. Closing
// the listener removes the socket.
func Listen(path, group string, log *slog.Logger) (net.Listener, error) {
	gid, mode := -1, os.FileMode(0o600)
	g, err := user.LookupGroup(group)
	if err == nil {
		gid, err = strconv.Atoi(g.Gid)
	}
	if err != nil {
		log.Warn("control socket is root-only: the web panel cannot connect until the group exists "+
			"and the daemon restarts, see the operations guide", "group", group, "error", err)
	} else {
		mode = 0o660
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("control socket %s: another process serves it", path)
	}
	// Bind inside a directory only root can enter and move the socket into
	// place once its mode and group are set, so it is never reachable with
	// the wrong ones. The rename replaces a stale socket in one step.
	private, err := os.MkdirTemp(dir, ".control-")
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	defer os.RemoveAll(private)
	tmp := filepath.Join(private, "control.sock")
	ln, err := net.Listen("unix", tmp)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	// The socket moves, so Close removes it by its final path instead.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	err = os.Chmod(tmp, mode)
	if err == nil && gid >= 0 {
		err = os.Chown(tmp, -1, gid)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("control socket: %w", err)
	}
	return &listener{Listener: ln, path: path}, nil
}

// listener removes its socket when closed.
type listener struct {
	net.Listener
	path string
	once sync.Once
	err  error
}

func (l *listener) Close() error {
	l.once.Do(func() {
		os.Remove(l.path)
		l.err = l.Listener.Close()
	})
	return l.err
}

// Serve answers the API on ln until ctx is done, then closes ln. Every
// request's context derives from ctx, so open Watch streams end with it
// and the shutdown does not wait for them.
func Serve(ctx context.Context, ln net.Listener, src Source) error {
	defer ln.Close()
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler: Handler(src), Protocols: p, ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
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
		srv.Close() // a handler that ignores its context
	}
	<-served // http.ErrServerClosed once Shutdown has begun
	return err
}
