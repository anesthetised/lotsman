package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/store"
)

// A replacement provider config that cannot be read, parsed, validated or
// resolved is reported, and the running upstream keeps its device and peers.
func TestReloadKeepsUpstreamWhenReplacementFails(t *testing.T) {
	h := newHarness(t)
	h.st.CreateUser("alice")
	peer, _ := h.st.AddPeer("alice", store.DefaultDevice, "nl", h.cfg.Subnet)
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
