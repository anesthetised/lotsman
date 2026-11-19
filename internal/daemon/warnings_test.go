package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/store"
)

// Warnings never stop the daemon from starting or reloading, but the
// operator must see them at startup and on every reload.
func TestProfileWarningsAreLoggedAtStartAndReload(t *testing.T) {
	h := newHarness(t)
	var logs bytes.Buffer
	h.d.log = slog.New(slog.NewTextHandler(&logs, nil))
	// fi matches no upstream; the NL tier of the profile is still usable.
	if err := h.st.AddProfile(config.Profile{Name: "startup", Prefer: []config.Match{{Geo: "NL"}, {Geo: "FI"}}}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	t.Cleanup(h.d.stop)
	if err := h.d.start(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `profile \"startup\": tier 2`) {
		t.Errorf("startup log lacks the warning:\n%s", logs.String())
	}

	logs.Reset()
	if err := h.st.AddProfile(config.Profile{Name: "later", Prefer: []config.Match{{Geo: "SE"}, {Geo: "DE"}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reload(ctx, h.cfg); err != nil {
		t.Fatalf("a warning must not fail the reload: %v", err)
	}
	if out := logs.String(); !strings.Contains(out, `level=WARN`) || !strings.Contains(out, `profile \"later\": tier 1`) || !strings.Contains(out, "configuration reloaded") {
		t.Errorf("reload log lacks the warning or the success line:\n%s", out)
	}
}

// A stored profile that no upstream of the config can serve does not stop
// the start: it is logged and its peers stay blocked. A reload that changes
// nothing about it is not refused either.
func TestStartKeepsGoingWithProfileWithoutUpstream(t *testing.T) {
	h := newHarness(t)
	logs := &lockedBuffer{}
	h.d.log = slog.New(slog.NewTextHandler(logs, nil))
	if err := h.st.AddProfile(config.Profile{Name: "fi", Prefer: []config.Match{{Geo: "FI"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.CreateUser("bob"); err != nil {
		t.Fatal(err)
	}
	p, err := h.st.AddPeer("bob", store.DefaultDevice, "fi", h.cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.d.stop)
	ctx := context.Background()
	if err := h.d.start(ctx); err != nil {
		t.Fatalf("start = %v", err)
	}
	if !strings.Contains(logs.String(), `profile has no upstream to use`) || !strings.Contains(logs.String(), `profile \"fi\": prefer matches no upstream`) {
		t.Errorf("start log lacks the warning:\n%s", logs)
	}
	if err := h.d.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if h.dp.IsAllowed(p.IP) {
		t.Error("a peer of a profile without upstream is admitted")
	}
	if err := h.d.Reload(ctx, h.cfg); err != nil {
		t.Errorf("reload = %v, want nil: the running config does not serve fi either", err)
	}
}
