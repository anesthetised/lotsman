package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeWebConfig writes a web.yaml for a free loopback port and returns its
// path and the panel's URL.
func writeWebConfig(t *testing.T, extra string) (path, url string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	dir := t.TempDir()
	path = filepath.Join(dir, "web.yaml")
	os.WriteFile(path, []byte("listen: "+addr+"\npublic_url: http://"+addr+"\nstate_dir: "+dir+"\n"+extra), 0o640)
	return path, "http://" + addr
}

// The global -config names web.yaml for web; without it, $LOTSMAN_WEB_CONFIG
// does, never lotsman.yaml's $LOTSMAN_CONFIG.
func TestWebConfigPath(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "web.yaml")
	os.WriteFile(bad, []byte("public_url: http://vpn.example.com\n"), 0o640)
	t.Setenv("LOTSMAN_CONFIG", testConfig(t))
	if _, err := capture(t, "-config", bad, "web"); err == nil || !strings.Contains(err.Error(), "public_url") {
		t.Errorf("-config: err = %v, want the web config's", err)
	}
	t.Setenv("LOTSMAN_WEB_CONFIG", bad)
	if _, err := capture(t, "web"); err == nil || !strings.Contains(err.Error(), "public_url") {
		t.Errorf("$LOTSMAN_WEB_CONFIG: err = %v, want the web config's", err)
	}
}

func TestWebServesUntilStopped(t *testing.T) {
	path, url := writeWebConfig(t, "socket: "+filepath.Join(t.TempDir(), "none.sock")+"\n")
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runWeb(ctx, path, slog.LevelError) }()
	var res *http.Response
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if res, err = http.Get(url + "/"); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Security-Policy") == "" || len(body) == 0 {
		t.Errorf("GET /: %d %q %q", res.StatusCode, res.Header, body)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "web.db")); err != nil {
		t.Errorf("web.db: %v", err)
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("web: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("web still running after stop")
	}
}
