//go:build linux && integration

package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
)

// freezeAfter is where a frozen path stops a flow, as DPI that stalls TCP
// to foreign hosting after its first 15–20 KB does.
const freezeAfter = 16 << 10

// serveDownloads answers HTTP on ln with a 1 MiB body. While frozen is set,
// each response stops after freezeAfter bytes and the connection stays
// open, silent, until the client gives up: the handshake and the first
// kilobytes pass, the rest never comes.
func serveDownloads(t *testing.T, ln net.Listener, frozen *atomic.Bool) {
	t.Helper()
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 4096)
		for sent := 0; sent < 1<<20; sent += len(chunk) {
			if frozen.Load() && sent >= freezeAfter {
				<-r.Context().Done()
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// metricValue returns the value of one series in a scrape, such as
// `lotsman_upstream_degraded{upstream="nl"}`.
func metricValue(body, series string) (float64, bool) {
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)$`).FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

// An AWG upstream whose path freezes every flow after 16 KiB still passes
// its reachability probe; the transfer probe marks it degraded, the client
// fails over to the Outline upstream, whose transfer probes pass through the
// proxy stack, and comes back once the path recovers.
func TestEndToEndTransfer(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates TUNs and programs the kernel")
	}
	dir := t.TempDir()
	nl := newProvider(t, "nl", netip.MustParsePrefix("10.100.0.0/24"), 51001)
	os.WriteFile(filepath.Join(dir, "nl.conf"), []byte(nl.conf), 0o600)
	var nlFrozen, olFrozen atomic.Bool // ol's path never freezes
	nlHTTP, err := nl.tnet.ListenTCPAddrPort(netip.AddrPortFrom(internet, 80))
	if err != nil {
		t.Fatal(err)
	}
	serveDownloads(t, nlHTTP, &nlFrozen)

	listen := func() net.Listener {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return ln
	}
	olName, olHTTP := listen(), listen()
	proxytest.Serve(t, olName, func(c net.Conn) { c.Write([]byte("ol")) })
	serveDownloads(t, olHTTP, &olFrozen)
	ss := proxytest.NewServer(t, "outline-secret", map[string]string{
		netip.AddrPortFrom(internet, 443).String(): olName.Addr().String(),
		netip.AddrPortFrom(internet, 80).String():  olHTTP.Addr().String(),
		netip.AddrPortFrom(internet, 53).String():  proxytest.DNS(t).String(),
	})
	os.WriteFile(filepath.Join(dir, "ol.key"), []byte(ss.Key()+"\n"), 0o600)

	cfgPath := filepath.Join(dir, "lotsman.yaml")
	os.WriteFile(cfgPath, []byte(`
listen: 127.0.0.1:51820
endpoint: 127.0.0.1:51820
state_dir: `+filepath.Join(dir, "state")+`
upstreams:
  - {name: nl, conf: `+filepath.Join(dir, "nl.conf")+`, geo: NL, provider: awg}
  - {name: ol, kind: outline, conf: `+filepath.Join(dir, "ol.key")+`, geo: FI, provider: outline}
health:
  interval: 500ms
  probe: 198.51.100.1:443
  dns: 198.51.100.1:53
  down_after: 2
  up_after: 1
  transfer: {url: 'http://198.51.100.1/down', min_bytes: 65536, timeout: 1s, interval: 1s}
metrics: {listen: 127.0.0.1:19100}
`), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, cfgPath, slog.LevelInfo) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	eventually(t, "daemon status", 3*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "state", "status.json"))
		return err == nil
	})
	out, err := capture(t, "-config", cfgPath, "device", "add", "alice/phone", "NL", "FI")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, out)
	exitsVia := func(want string) func() bool {
		return func() bool { got, err := c.exitVia(2 * time.Second); return err == nil && got == want }
	}
	metricIs := func(series string, want float64) func() bool {
		return func() bool {
			body, _, err := scrape()
			v, ok := metricValue(body, series)
			return err == nil && ok && v == want
		}
	}
	passed := func(name string) func() bool {
		return func() bool {
			body, _, err := scrape()
			v, ok := metricValue(body, `lotsman_upstream_transfer_probes_total{upstream="`+name+`",result="ok"}`)
			return err == nil && ok && v > 0
		}
	}

	eventually(t, "exit via nl", 15*time.Second, exitsVia("nl"))
	eventually(t, "transfer probes pass through nl (AWG)", 15*time.Second, passed("nl"))
	eventually(t, "transfer probes pass through ol (Outline)", 15*time.Second, passed("ol"))

	const nlFailed = `lotsman_upstream_probes_total{upstream="nl",result="fail"}`
	body, _, err := scrape()
	failedBefore, ok := metricValue(body, nlFailed)
	if err != nil || !ok {
		t.Fatalf("scrape: %v\n%s", err, body)
	}

	// The path through nl freezes: reachability holds, throughput does not.
	nlFrozen.Store(true)
	eventually(t, "nl degraded", 20*time.Second, metricIs(`lotsman_upstream_degraded{upstream="nl"}`, 1))
	if body, _, _ := scrape(); !strings.Contains(body, `lotsman_upstream_up{upstream="nl"} 1`) ||
		!strings.Contains(body, `lotsman_upstream_degraded{upstream="ol"} 0`) {
		t.Errorf("nl must stay reachable and ol undegraded:\n%s", body)
	}
	eventually(t, "exit via ol while nl is frozen", 15*time.Second, exitsVia("ol"))
	eventually(t, "status shows nl degraded", 10*time.Second, func() bool {
		status, _ := capture(t, "-config", cfgPath, "upstream", "status")
		f := tableRow(status, "nl")
		return len(f) > 3 && f[3] == "degraded"
	})

	// ol is cut off too: slow connectivity beats none, so the client falls
	// back to the degraded nl, and returns to ol once it recovers.
	restoreOL := blockOutline(t, ss)
	eventually(t, "ol down", 15*time.Second, metricIs(`lotsman_upstream_up{upstream="ol"}`, 0))
	eventually(t, "exit via degraded nl as the last resort", 15*time.Second, exitsVia("nl"))
	restoreOL()
	eventually(t, "back via ol while nl is still frozen", 15*time.Second, exitsVia("ol"))

	// The path recovers: nl is trusted again and preferred.
	nlFrozen.Store(false)
	eventually(t, "nl recovered", 20*time.Second, metricIs(`lotsman_upstream_degraded{upstream="nl"}`, 0))
	eventually(t, "back via nl", 15*time.Second, exitsVia("nl"))
	body, _, _ = scrape()
	if failed, _ := metricValue(body, nlFailed); failed != failedBefore {
		t.Errorf("nl failed %v reachability probes while frozen; it must stay reachable", failed-failedBefore)
	}
}
