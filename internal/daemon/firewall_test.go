package daemon

import (
	"net/netip"
	"testing"
	"time"
)

// The dataplane opens the metrics endpoint to clients, so it must learn it.
func TestSetupGetsMetricsEndpoint(t *testing.T) {
	h := newHarness(t)
	h.cfg.Metrics.Listen = "10.77.0.1:9100"
	h.run(t)
	select {
	case <-h.d.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("daemon not ready")
	}
	if want := netip.MustParseAddrPort("10.77.0.1:9100"); h.dp.Metrics != want {
		t.Errorf("Setup metrics = %v, want %v", h.dp.Metrics, want)
	}
}
