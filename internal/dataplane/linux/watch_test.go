//go:build linux && integration

package linux

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestWatch checks which kernel changes Watch reports and that it stops
// when told to (issue #81). Like the repair test, it touches nothing outside
// Lotsman's own objects and test tables.
func TestWatch(t *testing.T) {
	d := packetNetwork(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := d.Watch(ctx)

	nft := func(script string) {
		t.Helper()
		cmd := exec.Command("nft", "-f", "-")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("nft: %v: %s", err, out)
		}
	}
	t.Cleanup(func() {
		exec.Command("nft", "delete", "table", "ip", "lmwatch").Run()
		exec.Command("nft", "delete", "table", "ip", "lmtest").Run()
	})
	reported := func(within time.Duration) bool {
		t.Helper()
		select {
		case err, ok := <-changes:
			if !ok {
				t.Fatal("changes closed early")
			}
			if err != nil {
				t.Fatalf("watch failed: %v", err)
			}
			return true
		case <-time.After(within):
			return false
		}
	}
	// quiet drains reports until none arrives for a while.
	quiet := func() {
		t.Helper()
		for reported(300 * time.Millisecond) {
		}
	}

	if !reported(2 * time.Second) {
		t.Fatal("no report when the watch started")
	}
	quiet()

	nft("table ip lmwatch {\n chain c {\n }\n}")
	if reported(300 * time.Millisecond) {
		t.Error("a foreign table without a forward chain was reported")
	}

	for _, tc := range []struct{ name, script string }{
		{"table deleted", "delete table inet lotsman"},
		// Like Docker starting after Lotsman.
		{"foreign forward chain added", "table ip lmtest {\n chain forward {\n  type filter hook forward priority 0; policy drop;\n }\n}"},
		{"foreign accepts removed", "flush chain ip lmtest forward"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nft(tc.script)
			if !reported(2 * time.Second) {
				t.Fatal("not reported")
			}
			if repaired, err := d.Verify(); !repaired || err != nil {
				t.Errorf("Verify = %v, %v; want a repair", repaired, err)
			}
			quiet() // the repair's own changes
			if repaired, err := d.Verify(); repaired || err != nil {
				t.Errorf("Verify after the repair = %v, %v; want intact", repaired, err)
			}
			if reported(300 * time.Millisecond) {
				t.Error("a Verify that changed nothing was reported")
			}
		})
	}

	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-changes:
			if !ok {
				return // the monitor and its goroutines are gone
			}
		case <-deadline:
			t.Fatal("changes not closed after cancel")
		}
	}
}
