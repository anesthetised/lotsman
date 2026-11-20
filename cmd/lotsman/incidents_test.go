package main

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/store"
)

func TestIncidentsCommand(t *testing.T) {
	path := testConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.StateDir, 0o700)
	st, err := store.Open(dbPath(cfg.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	closed, _ := st.OpenIncident("nl", store.IncidentDownload, start, "16384 of 65536 bytes", 3)
	st.CloseIncident(closed, start.Add(90*time.Second))
	st.OpenIncident("de", store.IncidentDown, start.Add(-time.Hour), "probe failed: connection refused", 1)
	st.Close()

	out := mustRun(t, "-config", path, "incidents")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "UPSTREAM") {
		t.Fatalf("incidents:\n%s", out)
	}
	// Open first, though older; the detail keeps its spaces.
	if f := strings.Fields(lines[1]); !slices.Equal(f[:2], []string{"de", "down"}) || f[3] != "ongoing" || f[4] != "1" ||
		!strings.HasSuffix(lines[1], "probe failed: connection refused") {
		t.Errorf("first row %q", lines[1])
	}
	if f := strings.Fields(lines[2]); !slices.Equal(f, []string{"nl", "download", start.Local().Format("2006-01-02T15:04:05"), "1m30s", "3", "16384", "of", "65536", "bytes"}) {
		t.Errorf("second row %q", f)
	}

	out = mustRun(t, "-config", path, "incidents", "-upstream", "nl")
	if strings.Contains(out, "de ") || !strings.Contains(out, "download") {
		t.Errorf("-upstream nl:\n%s", out)
	}
	out = mustRun(t, "-config", path, "incidents", "-n", "1")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[1], "de") {
		t.Errorf("-n 1:\n%s", out)
	}
	for _, args := range [][]string{{"-n", "0"}, {"nl"}, {"-bogus"}} {
		if _, err := capture(t, append([]string{"-config", path, "incidents"}, args...)...); err == nil {
			t.Errorf("incidents %q accepted", args)
		}
	}
}
