package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestReloadChangesUDPGSOOnlyOnAffectedAWGLink(t *testing.T) {
	h := newHarness(t)
	h.run(t)
	select {
	case <-h.d.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not become ready")
	}

	links := func() (link, link) {
		t.Helper()
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		if len(h.d.ups) != 2 || h.d.ups[0].Name != "nl" || h.d.ups[1].Name != "de" {
			t.Fatal("expected running nl and de upstreams")
		}
		return h.d.ups[0].link, h.d.ups[1].link
	}
	reload := func(disabled bool) {
		t.Helper()
		cfg := *h.cfg
		cfg.Upstreams = slices.Clone(cfg.Upstreams)
		cfg.Upstreams[0].DisableUDPGSO = disabled
		if err := h.d.Reload(context.Background(), &cfg); err != nil {
			t.Fatal(err)
		}
	}

	nl, de := links()
	for _, disabled := range []bool{true, false} {
		reload(disabled)
		nextNL, nextDE := links()
		if nextNL == nl || nextDE != de {
			t.Fatalf("disable_udp_gso=%t: changed links nl=%t de=%t, want only nl", disabled, nextNL != nl, nextDE != de)
		}
		reload(disabled)
		if gotNL, gotDE := links(); gotNL != nextNL || gotDE != de {
			t.Fatal("reloading the same setting replaced a running link")
		}
		nl = nextNL
	}
}

// A replacement provider config that cannot be read, parsed, validated or
// resolved is reported, and the running upstream keeps its device and peers.
func TestReloadKeepsUpstreamWhenReplacementFails(t *testing.T) {
	h := newHarness(t)
	h.st.CreateUser("alice")
	peer, _ := h.st.AddPeer("alice", "nl", nlRoute, h.cfg.Subnet)
	h.run(t)
	routedViaNL := func() bool { got, _ := h.dp.RouteOf(peer.IP); return got == "lm-up-nl" }
	eventually(t, "initial routing", routedViaNL)
	path := filepath.Join(h.dir, "nl.conf")
	good := mustRead(t, path)
	h.d.mu.Lock()
	device := h.d.ups[0].link
	h.d.mu.Unlock()
	kept := func() bool {
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		return len(h.d.ups) == 2 && h.d.ups[0].Name == "nl" && h.d.ups[0].link == device
	}

	write := func(content string) func(*testing.T) {
		return func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name    string
		corrupt func(*testing.T)
	}{
		{"parse error", write("[Bogus]\n")},
		{"validation error", write(strings.Replace(good, "0.0.0.0/0", "10.0.0.0/8", 1))},
		{"unresolvable endpoint", write(strings.Replace(good, "localhost:51820", "vpn.provider.test:51820", 1))},
		{"unreadable file", func(t *testing.T) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.corrupt(t)
			if err := h.d.Reload(context.Background(), h.cfg); err == nil || !strings.Contains(err.Error(), "upstream nl:") {
				t.Errorf("Reload error = %v, want upstream nl", err)
			}
			if !kept() {
				t.Error("running nl upstream was dropped")
			}
			if !routedViaNL() {
				t.Error("peer lost its route through nl")
			}
			names := []string{}
			for _, u := range h.dp.Ups {
				names = append(names, u.Name)
			}
			if strings.Join(names, ",") != "lm-up-nl,lm-up-de" {
				t.Errorf("dataplane upstreams = %v", names)
			}
		})
	}

	// The restored file matches what is running: nothing is replaced.
	write(good)(t)
	if err := h.d.Reload(context.Background(), h.cfg); err != nil {
		t.Fatal(err)
	}
	if !kept() || !routedViaNL() {
		t.Error("restored provider config replaced nl or moved its peer")
	}
}
