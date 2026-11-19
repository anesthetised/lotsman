package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/web"
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
	_, port, _ := net.SplitHostPort(addr)
	os.WriteFile(path, []byte("listen: "+addr+"\npublic_url: http://localhost:"+port+"\nstate_dir: "+dir+"\n"+extra), 0o640)
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

// web enroll prints a one-time link on the public URL and stores only the
// token's hash; the link enrolls once.
func TestWebEnroll(t *testing.T) {
	path, _ := writeWebConfig(t, "")
	t.Setenv("LOTSMAN_WEB_CONFIG", path)
	out := mustRun(t, "web", "enroll")
	cfg, err := web.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	_, link, ok := strings.Cut(out, cfg.Origin()+"/enroll#")
	token := strings.TrimSpace(link)
	if !ok || len(token) != 43 || !strings.Contains(out, "works once") {
		t.Fatalf("output %q", out)
	}
	db, err := web.OpenDB(filepath.Join(cfg.StateDir, "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var hash []byte
	var expires int64
	if err := db.QueryRow(`SELECT token_hash, expires_at FROM enrollments`).Scan(&hash, &expires); err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256([]byte(token)); !bytes.Equal(hash, want[:]) {
		t.Error("web.db does not hold the token's hash")
	}
	if left := time.Until(time.Unix(expires, 0)); left <= web.EnrollmentTTL-time.Minute || left > web.EnrollmentTTL {
		t.Errorf("expires in %s", left)
	}
	if _, err := capture(t, "web", "enroll", "now"); err == nil {
		t.Error("web enroll takes no arguments")
	}
}
