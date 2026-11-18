//go:build linux && integration

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
)

// TestEndToEndOutlineWebSocket runs an Outline upstream whose key carries
// Shadowsocks TCP and UDP over WebSockets (wss://, as outline-ss-server
// serves them) through a helper process: the daemon resolves the URL's host
// once, the helper verifies the server's certificate for that name against
// the roots the daemon was given, and health passes over both.
func TestEndToEndOutlineWebSocket(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates TUNs and programs the kernel")
	}
	dir := t.TempDir()
	ss, _ := newOutline(t)
	front := ss.WebSocket(t, true)

	// The helper runs as an unprivileged user and must read the CA; t.TempDir is
	// private to root. Go reads SSL_CERT_FILE when it first loads the
	// system roots, which a helper does in a fresh process.
	caDir, err := os.MkdirTemp("", "lotsman-ca-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(caDir) })
	os.Chmod(caDir, 0o755)
	ca := filepath.Join(caDir, "ca.pem")
	if err := os.WriteFile(ca, front.CAPEM(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", ca)
	os.WriteFile(filepath.Join(dir, "ol.key"), []byte(front.Config("localhost")), 0o600)

	cfgPath := filepath.Join(dir, "lotsman.yaml")
	os.WriteFile(cfgPath, []byte(`
listen: 127.0.0.1:51820
endpoint: 127.0.0.1:51820
state_dir: `+filepath.Join(dir, "state")+`
upstreams:
  - {name: ol, kind: outline, conf: `+filepath.Join(dir, "ol.key")+`, provider: outline}
profiles:
  - {name: p, prefer: [{provider: outline}]}
health: {interval: 500ms, probe: 198.51.100.1:443, dns: 198.51.100.1:53, down_after: 2, up_after: 1}
metrics: {listen: 127.0.0.1:19100}
`), 0o600)

	out, err := capture(t, "-config", cfgPath, "config", "check")
	endpoint := fmt.Sprintf("endpoint 127.0.0.1:%d", front.Addr().Port()) // localhost, resolved
	if err != nil || !strings.Contains(out, "lm-up-ol  kind outline  "+endpoint) || !strings.Contains(out, "over WebSocket") ||
		strings.Contains(out, "outline-secret") || strings.Contains(out, "s3cr3t") {
		t.Fatalf("config check: %v\n%s", err, out)
	}

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
	out, err = capture(t, "-config", cfgPath, "user", "add", "alice", "-profile", "p")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, out)
	exitsViaOL := func() bool { got, err := c.exitVia(2 * time.Second); return err == nil && got == "ol" }

	// TCP and UDP from the client leave through the WebSockets.
	eventually(t, "exit via ol", 15*time.Second, exitsViaOL)
	if err := c.dnsVia(1); err != nil {
		t.Fatalf("udp through ol: %v", err)
	}
	if seen := strings.Join(ss.Seen(), ","); !strings.Contains(seen, "udp 198.51.100.1:53") || !strings.Contains(seen, "tcp 198.51.100.1:443") {
		t.Errorf("outline server saw %s", seen)
	}
	paths := map[string]bool{}
	for _, h := range front.Handshakes() {
		paths[h.Path] = true
		if h.SNI != "localhost" || h.Host != fmt.Sprintf("localhost:%d", front.Addr().Port()) {
			t.Errorf("handshake %+v, want the URL's host as SNI and Host", h)
		}
	}
	if !paths[proxytest.TCPPath] || !paths[proxytest.UDPPath] {
		t.Errorf("websocket paths used: %v", paths)
	}

	// The WebSocket port blocked: health fails, the client is cut off;
	// unblocked, it comes back.
	restore := blockLoopback(t, "tcp", front.Addr().Port())
	eventually(t, "ol down", 15*time.Second, func() bool {
		body, _, err := scrape()
		return err == nil && strings.Contains(body, `lotsman_upstream_up{upstream="ol"} 0`)
	})
	if _, err := c.exitVia(time.Second); err == nil {
		t.Error("client still reaches the internet with its upstream down")
	}
	restore()
	eventually(t, "back via ol", 15*time.Second, exitsViaOL)
	if err := c.dnsVia(2); err != nil {
		t.Errorf("udp through ol after recovery: %v", err)
	}
}
