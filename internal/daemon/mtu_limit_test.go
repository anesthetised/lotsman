package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientMTUUsesInterfaceAndOuterLimitsAtStartupAndReload(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(h.dir, "nl.conf")
	provider := strings.NewReplacer("MTU = 1400\n", "MTU = 1280\n", "S4 = 8\n", "S4 = 20\n").Replace(mustRead(t, path))
	if err := os.WriteFile(path, []byte(provider), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.d.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.d.stop)
	if client := h.d.ClientMTU(); client != 1280 || h.dp.MTU != 1280 {
		t.Fatalf("client MTU / dataplane MTU at startup = %d / %d, want 1280", client, h.dp.MTU)
	}
	padded := strings.Replace(provider, "S4 = 20\n", "S4 = 20\nContentPaddingAddition = 0-160\n", 1)
	if err := os.WriteFile(path, []byte(padded), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reload(context.Background(), h.cfg); err != nil {
		t.Fatal(err)
	}
	if client := h.d.ClientMTU(); client != 1240 || h.dp.MTU != 1240 { // 1500 - 80 - 20 - 160
		t.Errorf("client MTU / dataplane MTU after reload = %d / %d, want 1240", client, h.dp.MTU)
	}
}
