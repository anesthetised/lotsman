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
	"github.com/anesthetised/lotsman/internal/store"
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

func TestConfigCheckPrintsUploadSpeedAndIncidents(t *testing.T) {
	path := testConfig(t)
	out, err := capture(t, "-config", path, "config", "check")
	if err != nil || strings.Contains(out, "speed") || !strings.Contains(out, "incidents send_errors_per_minute 5\n") {
		t.Fatalf("defaults: %v\n%s", err, out)
	}
	cfg := mustReadFile(t, path) + `health:
  transfer:
    url: https://speed.example/down
    upload_url: https://speed.example/up
  speed:
    down_url: https://speed.example/down?bytes=2000000
    up_url: https://speed.example/up
incidents:
  send_errors_per_minute: 20
`
	os.WriteFile(path, []byte(cfg), 0o600)
	out, err = capture(t, "-config", path, "config", "check")
	for _, want := range []string{
		"transfer url https://speed.example/down  min_bytes 65536  upload_url https://speed.example/up  upload_bytes 262144  timeout 10s",
		"speed    down_url https://speed.example/down?bytes=2000000  up_url https://speed.example/up  bytes 2000000  timeout 30s  interval 30m0s ±20%",
		"incidents send_errors_per_minute 20",
	} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("config check: %v, want %q in\n%s", err, want, out)
		}
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
		{Name: "nl", Kind: "awg", Interface: "lm-up-nl", State: health.Up, Degraded: true,
			Incidents: []store.IncidentKind{store.IncidentUpload, store.IncidentSendErrors}},
		{Name: "de", Kind: "awg", Interface: "lm-up-de", State: health.Down, Degraded: true,
			Incidents: []store.IncidentKind{store.IncidentDown}},
		{Name: "fi", Kind: "awg", Interface: "lm-up-fi", State: health.Up,
			Speed: health.Speed{Down: 12_500_000, Up: 625_000, Time: time.Now()}},
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
	// The last two columns: the speed, down/up in Mbit/s, and the open incidents.
	for name, want := range map[string][2]string{"nl": {"-", "upload,send_errors"}, "de": {"-", "down"}, "fi": {"100.0/5.0", "-"}} {
		if f := tableFields(out, name); len(f) < 2 || f[len(f)-2] != want[0] || f[len(f)-1] != want[1] {
			t.Errorf("%s row = %q, want speed %s and incidents %s:\n%s", name, f, want[0], want[1], out)
		}
	}
}
