package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/config"
)

// Warnings never stop a configuration from loading, but the operator must
// see them at startup and on every reload.
func TestConfigWarningsAreLoggedAtStartAndReload(t *testing.T) {
	h := newHarness(t)
	var logs bytes.Buffer
	h.d.log = slog.New(slog.NewTextHandler(&logs, nil))
	withProfile := func(p config.Profile) *config.Config {
		cfg := *h.cfg
		cfg.Profiles = append(slices.Clone(cfg.Profiles), p)
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		return &cfg
	}
	// fi matches no upstream; the NL tier of the profile is still usable.
	h.d.cfg = withProfile(config.Profile{Name: "startup", Prefer: []config.Match{{Geo: "NL"}, {Geo: "FI"}}})

	ctx := context.Background()
	t.Cleanup(h.d.stop)
	if err := h.d.start(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `profile \"startup\": prefer[1]`) {
		t.Errorf("startup log lacks the warning:\n%s", logs.String())
	}

	logs.Reset()
	if err := h.d.Reload(ctx, withProfile(config.Profile{Name: "later", Prefer: []config.Match{{Geo: "SE"}, {Geo: "DE"}}})); err != nil {
		t.Fatalf("a warning must not fail the reload: %v", err)
	}
	if out := logs.String(); !strings.Contains(out, `level=WARN`) || !strings.Contains(out, `profile \"later\": prefer[0]`) || !strings.Contains(out, "configuration reloaded") {
		t.Errorf("reload log lacks the warning or the success line:\n%s", out)
	}
}
