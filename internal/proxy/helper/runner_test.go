package helper

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"github.com/anesthetised/lotsman/internal/proxy"
)

// testHelperEnv makes the test binary act as a helper; only the tests set it.
const testHelperEnv = "LOTSMAN_HELPER_TEST"

func TestMain(m *testing.M) {
	if mode := os.Getenv(testHelperEnv); mode != "" {
		os.Exit(testHelper(mode))
	}
	os.Exit(m.Run())
}

// testHelper is a helper on a netstack TUN, since a test cannot hand it a
// kernel one; it ignores fd 3. Modes: "serve" is the real thing; "log"
// writes a record and an overlong line first; "panic" panics on SIGUSR2;
// "fail" has no TUN; "hang" answers open and then nothing, not even the
// end of the control connection.
func testHelper(mode string) int {
	openTUN := func() (tun.Device, error) {
		dev, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil, 1500)
		return dev, err
	}
	switch mode {
	case "log":
		fmt.Fprintln(os.Stderr, `{"time":"2026-11-16T00:00:00Z","level":"WARN","msg":"from the helper","flows":3}`)
		fmt.Fprintln(os.Stderr, strings.Repeat("x", 3*maxLine))
	case "panic":
		usr2 := make(chan os.Signal, 1)
		signal.Notify(usr2, syscall.SIGUSR2)
		go func() {
			<-usr2
			panic("test-induced panic")
		}()
	case "fail":
		openTUN = func() (tun.Device, error) { return nil, errors.New("no tun in this test") }
	case "tunfail":
		openTUN = func() (tun.Device, error) {
			ft, err := newFailingTUN()
			if err == nil {
				time.AfterFunc(200*time.Millisecond, func() { close(ft.fail) })
			}
			return ft, err
		}
	case "hang":
		f := os.NewFile(controlFD, "control")
		var req request
		readFrame(f, &req)
		writeFrame(f, response{})
		time.Sleep(time.Hour)
	}
	return run(openTUN)
}

// fileTUN is a netstack TUN with a descriptor to pass, as a kernel TUN has.
type fileTUN struct {
	tun.Device
	f *os.File
}

func (t fileTUN) File() *os.File { return t.f }

func (t fileTUN) Close() error {
	t.f.Close()
	return t.Device.Close()
}

type harness struct {
	link   *link
	logs   *syncBuffer
	exited chan error
	spec   proxy.Spec
}

// newHarness starts a link whose helper is this test binary in mode.
func newHarness(t *testing.T, mode string, r Runner) *harness {
	t.Helper()
	r.Command = func() *exec.Cmd {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), testHelperEnv+"="+mode)
		return cmd
	}
	dev, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil, 1500)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{logs: &syncBuffer{}, exited: make(chan error, 10),
		spec: proxy.Spec{Key: "ss://Y2hhY2hhMjAtaWV0Zi1wb2x5MTMwNTpzZWNyZXQ@192.0.2.1:8388", Server: netip.MustParseAddrPort("192.0.2.1:8388"), MaxFlows: 7}}
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})).With("upstream", "ol")
	l, err := r.Start(fileTUN{dev, f}, h.spec, log, func(err error) { h.exited <- err })
	if err != nil {
		t.Fatal(err)
	}
	h.link = l.(*link)
	t.Cleanup(h.link.Close)
	return h
}

func (h *harness) pid() int {
	h.link.mu.Lock()
	defer h.link.mu.Unlock()
	if h.link.proc == nil {
		return 0
	}
	return h.link.proc.cmd.Process.Pid
}

// process returns the running helper.
func (h *harness) process(t *testing.T) *process {
	t.Helper()
	h.link.mu.Lock()
	defer h.link.mu.Unlock()
	if h.link.proc == nil {
		t.Fatal("no helper running")
	}
	return h.link.proc
}

func (h *harness) waitExit(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.exited:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("no exit reported")
		return nil
	}
}

func waitLog(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("log lacks %q:\n%s", want, logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunnerLifecycle(t *testing.T) {
	h := newHarness(t, "serve", Runner{})
	if _, err := h.link.Stats(); err == nil {
		t.Error("stats before Up")
	}
	if err := h.link.Up(); err != nil {
		t.Fatal(err)
	}
	p := h.process(t)
	argv := strings.Join(append(p.cmd.Args, p.cmd.Env...), " ")
	if strings.Contains(argv, "secret") || strings.Contains(argv, "Y2hhY2hh") {
		t.Error("the key is on the helper's command line or in its environment")
	}
	if st, err := h.link.Stats(); err != nil || st.MaxFlows != 7 {
		t.Errorf("stats = %+v, %v; want max flows 7 from the spec", st, err)
	}
	if err := h.link.SetMaxFlows(9); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.link.Stats(); st.MaxFlows != 9 {
		t.Errorf("max flows after SetMaxFlows = %d", st.MaxFlows)
	}
	if err := h.link.SetServer(h.spec.Key, netip.MustParseAddrPort("192.0.2.2:8388")); err != nil {
		t.Fatal(err)
	}
	// A rotated key travels with the address; one the helper cannot parse
	// is refused, and the running one stays for the next start.
	if err := h.link.SetServer("ss://Y2hhY2hhMjAtaWV0Zi1wb2x5MTMwNTpvdGhlcg@192.0.2.3:8388", netip.MustParseAddrPort("192.0.2.3:8388")); err != nil {
		t.Fatal(err)
	}
	if err := h.link.SetServer("ss://rc4-md5:x@192.0.2.4:8388", netip.MustParseAddrPort("192.0.2.4:8388")); err == nil {
		t.Error("the helper took a key it cannot use")
	}
	if h.link.spec.Server != netip.MustParseAddrPort("192.0.2.3:8388") || !strings.Contains(h.link.spec.Key, "@192.0.2.3:") {
		t.Errorf("spec after a refused change = %v", h.link.spec.Server)
	}
	if err := h.link.Up(); err != nil || h.process(t) != p {
		t.Errorf("a second Up = %v, started another helper %v", err, h.process(t) != p)
	}

	// Close tells it to exit and reaps it; that is not a crash.
	h.link.Close()
	select {
	case <-p.done:
	default:
		t.Fatal("Close returned before the helper was reaped")
	}
	if st := p.cmd.ProcessState; st == nil || !st.Exited() || st.ExitCode() != 0 {
		t.Errorf("helper ended %v, want exit 0", st)
	}
	select {
	case err := <-h.exited:
		t.Errorf("a closed link reported an exit: %v", err)
	default:
	}
	if err := h.link.Up(); err == nil {
		t.Error("Up after Close")
	}
	waitLog(t, h.logs, "proxy helper started")
}

func TestRunnerKilled(t *testing.T) {
	h := newHarness(t, "serve", Runner{Backoff: time.Second})
	if err := h.link.Up(); err != nil {
		t.Fatal(err)
	}
	h.link.SetMaxFlows(11) // the next helper starts with it
	first := h.pid()
	syscall.Kill(first, syscall.SIGKILL)
	if err := h.waitExit(t); err == nil || !strings.Contains(err.Error(), "signal: killed") {
		t.Errorf("exit reported as %v", err)
	}
	waitLog(t, h.logs, `msg="proxy helper exited" upstream=ol pid=`+fmt.Sprint(first)+` status="signal: killed"`)
	if _, err := h.link.Stats(); err == nil {
		t.Error("stats from a dead helper")
	}
	// It died soon after it started, so the restart waits.
	if err := h.link.Up(); !errors.Is(err, errBackoff) {
		t.Fatalf("Up right after a quick exit = %v, want the backoff", err)
	}
	time.Sleep(time.Second)
	if err := h.link.Up(); err != nil {
		t.Fatal(err)
	}
	if h.pid() == first {
		t.Error("same pid after restart")
	}
	if st, err := h.link.Stats(); err != nil || st.MaxFlows != 11 {
		t.Errorf("restarted helper stats = %+v, %v; want the latest max flows", st, err)
	}
}

// A panic is reported as an exit, its first line in the exit record and the
// whole trace relayed under the upstream's name.
func TestRunnerPanic(t *testing.T) {
	h := newHarness(t, "panic", Runner{})
	if err := h.link.Up(); err != nil {
		t.Fatal(err)
	}
	syscall.Kill(h.pid(), syscall.SIGUSR2)
	if err := h.waitExit(t); err == nil || !strings.Contains(err.Error(), "exit status 2") {
		t.Errorf("exit reported as %v", err)
	}
	waitLog(t, h.logs, `panic="panic: test-induced panic"`)
	waitLog(t, h.logs, `level=ERROR msg="proxy helper output" upstream=ol line="goroutine `)
}

// Records keep their level and attributes; overlong lines are cut.
func TestRunnerRelaysLogs(t *testing.T) {
	h := newHarness(t, "log", Runner{})
	if err := h.link.Up(); err != nil {
		t.Fatal(err)
	}
	waitLog(t, h.logs, `level=WARN msg="from the helper" upstream=ol flows=3`)
	waitLog(t, h.logs, "line="+strings.Repeat("x", maxLine)+"…\n")
	if strings.Contains(h.logs.String(), strings.Repeat("x", maxLine+1)) {
		t.Error("an overlong line was not cut")
	}
}

// A helper whose stack stopped because its TUN failed exits with an error,
// which counts as a crash: the upstream goes down and is restarted.
func TestRunnerTUNFailure(t *testing.T) {
	h := newHarness(t, "tunfail", Runner{})
	if err := h.link.Up(); err != nil {
		t.Fatal(err)
	}
	if err := h.waitExit(t); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("exit reported as %v", err)
	}
	waitLog(t, h.logs, "tun gone")
}

func TestRunnerOpenFails(t *testing.T) {
	h := newHarness(t, "fail", Runner{})
	if err := h.link.Up(); err == nil || !strings.Contains(err.Error(), "no tun in this test") {
		t.Fatalf("Up = %v", err)
	}
	if h.pid() != 0 {
		t.Error("a helper that failed to open counts as running")
	}
	select {
	case err := <-h.exited:
		t.Errorf("a helper that never came up reported an exit: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// A helper that stops answering is killed, and that counts as an exit; one
// that does not exit when told to is killed after StopTimeout.
func TestRunnerStuck(t *testing.T) {
	h := newHarness(t, "hang", Runner{RequestTimeout: 200 * time.Millisecond})
	if err := h.link.Up(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.link.Stats(); err == nil {
		t.Error("stats from a stuck helper")
	}
	if err := h.waitExit(t); err == nil || !strings.Contains(err.Error(), "signal: killed") {
		t.Errorf("exit reported as %v", err)
	}

	h = newHarness(t, "hang", Runner{StopTimeout: 300 * time.Millisecond})
	if err := h.link.Up(); err != nil {
		t.Fatal(err)
	}
	p := h.process(t)
	start := time.Now()
	h.link.Close()
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Errorf("Close took %v, before the stop timeout", took)
	}
	if st := p.cmd.ProcessState; st == nil || st.String() != "signal: killed" {
		t.Errorf("helper ended %v, want killed", st)
	}
}

func TestRunnerNeedsDescriptor(t *testing.T) {
	dev, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runner{}).Start(dev, proxy.Spec{}, slog.New(slog.DiscardHandler), nil); err == nil {
		t.Error("started a link on a TUN without a descriptor")
	}
}
