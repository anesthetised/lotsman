package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/health"
)

func TestConfigCheckPrintsTransfer(t *testing.T) {
	path := testConfig(t)
	out, err := capture(t, "-config", path, "config", "check")
	if err != nil || strings.Contains(out, "transfer") {
		t.Fatalf("transfer is off by default: %v\n%s", err, out)
	}
	cfg := mustReadFile(t, path) + "health:\n  transfer:\n    url: [https://speed.example/down, http://198.51.100.1/]\n    min_bytes: 131072\n"
	os.WriteFile(path, []byte(cfg), 0o600)
	out, err = capture(t, "-config", path, "config", "check")
	want := "transfer url https://speed.example/down, http://198.51.100.1/  min_bytes 131072  timeout 10s  interval 5m0s ±20%"
	if err != nil || !strings.Contains(out, want) {
		t.Errorf("config check: %v, want %q in\n%s", err, want, out)
	}
}

func TestUpstreamStatusShowsDegraded(t *testing.T) {
	path := testConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.StateDir, 0o700)
	s := statusFile{UpdatedAt: time.Now(), Upstreams: []daemon.UpstreamStatus{
		{Name: "nl", Kind: "awg", Interface: "lm-up-nl", State: health.Up, Degraded: true},
		{Name: "de", Kind: "awg", Interface: "lm-up-de", State: health.Down, Degraded: true},
		{Name: "fi", Kind: "awg", Interface: "lm-up-fi", State: health.Up},
	}}
	data, _ := json.Marshal(s)
	os.WriteFile(statusPath(cfg.StateDir), data, 0o600)
	out, err := capture(t, "-config", path, "upstream", "status")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"nl": "degraded", "de": "down", "fi": "up"} {
		if f := tableFields(out, name); len(f) < 4 || f[3] != want {
			t.Errorf("%s row = %q, want state %s:\n%s", name, f, want, out)
		}
	}
}
