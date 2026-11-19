//go:build linux && integration

package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/control"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/traffic"
)

// everyoneOperator signs every caller in as an operator; TestMain installs
// it in panel processes it starts with LOTSMAN_TEST_OPERATOR=1.
type everyoneOperator struct{}

func (everyoneOperator) Operator(*http.Request) error       { return nil }
func (everyoneOperator) User(*http.Request) (string, error) { return "", nil }

// panelSource is a daemon with one upstream. The test never calls the
// profile and connection services.
type panelSource struct {
	control.ProfileSource
	control.ConnectionSource
}

func (panelSource) Ready() <-chan struct{} {
	ready := make(chan struct{})
	close(ready)
	return ready
}
func (panelSource) ClientMTU() int { return 1280 }
func (panelSource) Status() daemon.Status {
	return daemon.Status{Upstreams: []daemon.UpstreamStatus{{Name: "nl", Kind: "awg"}}}
}
func (panelSource) Traffic() traffic.Snapshot { return traffic.Snapshot{} }

// systemUser makes sure the system user exists, creating it with the given
// useradd flags and removing it afterwards when the test created it.
func systemUser(t *testing.T, name string, flags ...string) *user.User {
	t.Helper()
	if _, err := user.Lookup(name); err != nil {
		args := append([]string{"--system", "--no-create-home", "--shell", "/usr/sbin/nologin"}, flags...)
		if out, err := exec.Command("useradd", append(args, name)...).CombinedOutput(); err != nil {
			t.Fatalf("useradd %s: %v: %s", name, err, out)
		}
		t.Cleanup(func() { exec.Command("userdel", name).Run() })
	}
	u, err := user.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestWebPanelSocketAccess runs lotsman web as the install sets it up, the
// unprivileged lotsman-web user in the lotsman-web group: it reaches the
// daemon's control socket and streams status through to the browser. The
// same panel run by a user outside the group cannot reach the socket.
func TestWebPanelSocketAccess(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates users and the control socket")
	}
	if _, err := user.LookupGroup(control.Group); err != nil {
		t.Skipf("needs the %s group, which deploy/Dockerfile.test creates: %v", control.Group, err)
	}
	panelUser := systemUser(t, "lotsman-web", "--gid", control.Group)
	outsider := systemUser(t, "lotsman-outsider", "--user-group")

	dir, err := os.MkdirTemp("", "lw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	os.Chmod(dir, 0o755) // as /run/lotsman: anyone may look up the socket
	socket := filepath.Join(dir, "control.sock")
	ln, err := control.Listen(socket, control.Group, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- control.Serve(ctx, ln, panelSource{}) }()
	t.Cleanup(func() { stop(); <-served })

	for _, tc := range []struct {
		name    string
		u       *user.User
		reaches bool
	}{
		{"lotsman-web", panelUser, true},
		{"outsider", outsider, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid, _ := strconv.Atoi(tc.u.Uid)
			gid, _ := strconv.Atoi(tc.u.Gid)
			origin, pid := startPanel(t, dir, tc.name, socket, uid, gid)
			st := procStatus(t, pid)
			if ids := tc.u.Uid + " " + tc.u.Uid + " " + tc.u.Uid + " " + tc.u.Uid; st["Uid"] != ids || st["CapEff"] != "0000000000000000" {
				t.Errorf("panel runs as Uid %q CapEff %q, want %q and no capabilities", st["Uid"], st["CapEff"], ids)
			}

			hc := &http.Client{Transport: originTransport{origin}}
			client := lotsmanv1connect.NewStatusServiceClient(connect.NewClient(connecthttp.NewTransport(hc, origin+"/api")))
			stream, err := client.Watch(t.Context(), &lotsmanv1.WatchStatusRequest{})
			var snap *lotsmanv1.StatusSnapshot
			if err == nil {
				snap, err = stream.Receive()
				stream.Close()
			}
			if tc.reaches {
				if err != nil || len(snap.Upstreams) != 1 || snap.Upstreams[0].Name != "nl" {
					t.Errorf("watch through the panel: %v, %v", snap, err)
				}
			} else if connect.CodeOf(err) != connect.CodeUnavailable {
				t.Errorf("watch through the panel: %v, want unavailable", err)
			}
		})
	}
}

// startPanel runs `lotsman web` as uid:gid, with no supplementary groups,
// and waits until it answers. It returns the panel's origin and pid.
func startPanel(t *testing.T, dir, name, socket string, uid, gid int) (string, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	state := filepath.Join(dir, name)
	os.Mkdir(state, 0o700)
	os.Chown(state, uid, gid)
	cfg := filepath.Join(dir, name+".yaml")
	os.WriteFile(cfg, []byte("listen: "+addr+"\npublic_url: http://"+addr+"\nsocket: "+socket+"\nstate_dir: "+state+"\n"), 0o644)

	// /proc/self/exe: the test binary's own directory is root's alone.
	cmd := exec.Command("/proc/self/exe", "-config", cfg, "web")
	cmd.Env = append(os.Environ(), "LOTSMAN_TEST_RUN=1", "LOTSMAN_TEST_OPERATOR=1")
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-exited
			t.Errorf("panel ignored SIGTERM")
		}
		if t.Failed() {
			t.Logf("panel log:\n%s", stderr.String())
		}
	})
	eventually(t, "the panel listening", 10*time.Second, func() bool {
		select {
		case <-exited:
			t.Fatalf("panel exited: %s", stderr.String())
		default:
		}
		res, err := http.Get("http://" + addr + "/")
		if err == nil {
			res.Body.Close()
		}
		return err == nil
	})
	return "http://" + addr, cmd.Process.Pid
}

type originTransport struct{ origin string }

func (o originTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Origin", o.origin)
	return http.DefaultTransport.RoundTrip(r)
}
