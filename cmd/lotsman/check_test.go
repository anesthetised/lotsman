package main

import (
	"os"
	"strings"
	"testing"
)

func TestConfigCheckPrintsProbeTargets(t *testing.T) {
	path := testConfig(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, "health: {probe: [1.1.1.1:443, 9.9.9.9:443]}\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := capture(t, "-config", path, "config", "check")
	if err != nil {
		t.Fatalf("config check: %v\n%s", err, out)
	}
	if !strings.Contains(out, "probe 1.1.1.1:443, 9.9.9.9:443") {
		t.Errorf("config check output lacks the probe targets:\n%s", out)
	}
}
