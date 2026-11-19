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

func TestConfigCheckPrintsWarnings(t *testing.T) {
	path := testConfig(t)
	// FI matches nothing, but the NL tier still applies: stored with a warning.
	var err error
	msg := captureStderr(t, func() { err = run([]string{"-config", path, "profile", "set", "nl", "geo=NL", "geo=FI"}) })
	const want = `warning: profile "nl": tier 2 geo=FI matches no upstream`
	if err != nil || !strings.Contains(msg, want) {
		t.Fatalf("profile set: %v, stderr %q", err, msg)
	}
	out, err := capture(t, "-config", path, "config", "check")
	if err != nil {
		t.Fatalf("config check: %v\n%s", err, out)
	}
	if !strings.Contains(out, want) || !strings.Contains(out, "profile  nl         prefer geo=NL geo=FI") || !strings.Contains(out, "config OK") {
		t.Errorf("config check output lacks the profile or its warning:\n%s", out)
	}
}

// Profiles live in the store; config check validates them against the
// upstreams as the daemon does, so removing the only upstream a profile can
// use fails the check.
func TestConfigCheckValidatesStoredProfiles(t *testing.T) {
	path := testConfig(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), "geo: NL}]", "geo: DE}]", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := capture(t, "-config", path, "config", "check")
	if err == nil || !strings.Contains(err.Error(), `profile "nl": prefer matches no upstream`) || strings.Contains(out, "config OK") {
		t.Errorf("config check = %v\n%s", err, out)
	}
}
