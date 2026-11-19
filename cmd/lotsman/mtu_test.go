package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/awgconf"
)

// Client configs leave room for an upstream's AWG 3 content padding.
func TestClientConfigSubtractsContentPadding(t *testing.T) {
	path := testConfig(t)
	providerPath := filepath.Join(filepath.Dir(path), "nl.conf")
	provider, err := os.ReadFile(providerPath)
	if err != nil {
		t.Fatal(err)
	}
	padded := strings.Replace(string(provider), "S4 = 8\n", "S4 = 8\nContentPaddingAddition = 0-64\n", 1)
	if padded == string(provider) {
		t.Fatal("provider fixture has no S4 = 8 line")
	}
	if err := os.WriteFile(providerPath, []byte(padded), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := capture(t, "-config", path, "device", "add", "alice/phone")
	if err != nil {
		t.Fatal(err)
	}
	if client, err := awgconf.Parse(strings.NewReader(out)); err != nil || client.Interface.MTU != 1392-64 {
		t.Errorf("client MTU = %v (%v), want %d", client.Interface.MTU, err, 1392-64)
	}
}
