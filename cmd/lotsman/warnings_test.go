package main

import (
	"os"
	"strings"
	"testing"
)

func TestConfigCheckPrintsWarnings(t *testing.T) {
	path := testConfig(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// FI matches nothing, but the NL tier still applies: loads with a warning.
	edited := strings.Replace(string(data), "prefer: [{geo: NL}]", "prefer: [{geo: NL}, {geo: FI}]", 1)
	if edited == string(data) {
		t.Fatal("test setup: profile not edited")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := capture(t, "-config", path, "config", "check")
	if err != nil {
		t.Fatalf("config check: %v\n%s", err, out)
	}
	if !strings.Contains(out, `warning: profile "nl": prefer[1] {Geo:FI Provider:} matches no upstream`) || !strings.Contains(out, "config OK") {
		t.Errorf("config check output lacks the warning:\n%s", out)
	}
}
