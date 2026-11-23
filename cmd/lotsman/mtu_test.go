package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/awgconf"
)

func TestClientConfigsKeepUpstreamInterfaceMTUWhenPaddingFits(t *testing.T) {
	path := testConfig(t)
	providerPath := filepath.Join(filepath.Dir(path), "nl.conf")
	provider, err := os.ReadFile(providerPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.NewReplacer("MTU = 1400\n", "MTU = 1280\n", "S4 = 8\n", "S4 = 20\n").Replace(string(provider))
	if !strings.Contains(updated, "MTU = 1280\n") || !strings.Contains(updated, "S4 = 20\n") {
		t.Fatal("could not configure the provider fixture")
	}
	if err := os.WriteFile(providerPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"add", "show"} {
		out, err := capture(t, "-config", path, "device", action, "alice/phone")
		if err != nil {
			t.Fatal(err)
		}
		client, err := awgconf.Parse(strings.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		if got := client.Interface.MTU; got != 1280 {
			t.Errorf("device %s MTU = %d, want 1280", action, got)
		}
	}
}

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
	client, err := awgconf.Parse(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if got := client.Interface.MTU; got != 1348 { // 1500 - 80 - 8 - 64
		t.Errorf("client MTU = %d, want 1348", got)
	}
}
