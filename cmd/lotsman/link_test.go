package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/store"
)

func TestUserLink(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv("LOTSMAN_WEB_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	mustRun(t, "-config", cfg, "device", "add", "alice/phone")
	resolve := func(token string) (string, error) {
		e, err := openEnv(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer e.store.Close()
		return e.store.ResolveLink(token)
	}
	link := func(args ...string) (stdout, stderr string, err error) {
		stderr = captureStderr(t, func() {
			stdout, err = capture(t, append([]string{"-config", cfg, "user", "link"}, args...)...)
		})
		return stdout, stderr, err
	}

	// Without a known public URL, only the token.
	out, errOut, err := link("alice")
	if err != nil {
		t.Fatal(err)
	}
	first := strings.TrimSpace(out)
	if user, err := resolve(first); err != nil || user != "alice" {
		t.Fatalf("printed %q: resolves to %q, %v", out, user, err)
	}
	if strings.Contains(errOut, first) {
		t.Errorf("stderr holds the token: %s", errOut)
	}

	// -url, else the panel's public_url.
	out, errOut, err = link("alice", "-url", "https://vpn.example.com/")
	second, ok := strings.CutPrefix(strings.TrimSpace(out), "https://vpn.example.com/u#")
	if err != nil || !ok || !store.ValidToken(second) || strings.Contains(errOut, second) {
		t.Fatalf("with -url: %q, %q, %v", out, errOut, err)
	}
	if _, err := resolve(first); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the first link survived a new one: %v", err)
	}
	webCfg := filepath.Join(t.TempDir(), "web.yaml")
	os.WriteFile(webCfg, []byte("public_url: https://panel.example.org:8443\n"), 0o600)
	t.Setenv("LOTSMAN_WEB_CONFIG", webCfg)
	out, _, err = link("alice")
	third, ok := strings.CutPrefix(strings.TrimSpace(out), "https://panel.example.org:8443/u#")
	if err != nil || !ok || !store.ValidToken(third) {
		t.Fatalf("with the panel's config: %q, %v", out, err)
	}

	if _, _, err := link("alice", "-revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(third); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the revoked link resolves: %v", err)
	}

	for _, args := range [][]string{
		{"bob"},
		{"bob", "-revoke"},
		{},
		{"alice", "extra"},
		{"alice", "-url", "ftp://vpn.example.com"},
		{"alice", "-url", "https://vpn.example.com/panel"},
	} {
		if _, _, err := link(args...); err == nil {
			t.Errorf("user link %q succeeded", args)
		}
	}
}
