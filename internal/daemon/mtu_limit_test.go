package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The client MTU leaves room for an upstream's AWG 3 content padding, both in
// the daemon and in the configs the CLI renders.
func TestClientMTUSubtractsContentPadding(t *testing.T) {
	h := newHarness(t)
	if err := h.d.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.d.stop)
	if got := h.d.ClientMTU(); got != 1392 { // nl: 1400-8
		t.Fatalf("ClientMTU before = %d, want 1392", got)
	}
	path := filepath.Join(h.dir, "nl.conf")
	padded := strings.Replace(mustRead(t, path), "S4 = 8\n", "S4 = 8\nContentPaddingAddition = 0-64\n", 1)
	if err := os.WriteFile(path, []byte(padded), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reload(context.Background(), h.cfg); err != nil {
		t.Fatal(err)
	}
	if got := h.d.ClientMTU(); got != 1392-64 {
		t.Errorf("ClientMTU after = %d, want %d", got, 1392-64)
	}
}
