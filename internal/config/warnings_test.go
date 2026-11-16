package config

import (
	"slices"
	"strings"
	"testing"
)

// profilesConfig is minimal with its profiles replaced. The upstreams are
// rs-nl (NL, redshield) and rs-de (DE, redshield).
func profilesConfig(profiles string) string {
	return replaceTopLevelField(minimal, "profiles", "profiles:\n"+profiles)
}

// Removing the only NL upstream must not fail the reload of a profile that
// still has a DE tier; the emptied tier is only reported.
func TestPreferTierEmptiedByUpstreamRemovalIsWarning(t *testing.T) {
	input := profilesConfig("  - {name: eu, prefer: [{geo: NL}, {geo: DE}]}\n")
	input = strings.Replace(input, "  - name: rs-nl\n    conf: /etc/lotsman/upstreams/rs-nl.conf\n    geo: NL\n    provider: redshield\n", "", 1)
	if strings.Contains(input, "rs-nl") {
		t.Fatal("test setup: rs-nl not removed")
	}
	cfg, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{`profile "eu": prefer[0] {Geo:NL Provider:} matches no upstream and is ignored`}
	if got := cfg.Warnings(); !slices.Equal(got, want) {
		t.Errorf("Warnings = %q, want %q", got, want)
	}
}

func TestProfileWithEveryTierEmptyIsError(t *testing.T) {
	for name, prefer := range map[string]string{
		"one entry":   "[{geo: XX}]",
		"two entries": "[{geo: XX}, {provider: nobody}]",
		"both fields": "[{geo: NL, provider: nobody}]",
		"typos":       "[{geo: NLL}, {geo: DEE}]",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(profilesConfig("  - {name: p, prefer: " + prefer + "}\n")))
			if err == nil || !strings.Contains(err.Error(), `profile "p": prefer matches no upstream`) {
				t.Fatalf("Parse error = %v", err)
			}
		})
	}
}

func TestShadowedPreferEntryIsWarningNamingShadower(t *testing.T) {
	// Every NL upstream is redshield, so prefer[1] has nothing left to take.
	cfg, err := Parse([]byte(profilesConfig("  - {name: p, prefer: [{provider: redshield}, {geo: NL}]}\n")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{`profile "p": prefer[1] {Geo:NL Provider:} never applies: every upstream it matches is taken by prefer[0] {Geo: Provider:redshield}`}
	if got := cfg.Warnings(); !slices.Equal(got, want) {
		t.Errorf("Warnings = %q, want %q", got, want)
	}
}

func TestShadowedPreferEntryNamesEveryShadower(t *testing.T) {
	cfg, err := Parse([]byte(profilesConfig("  - {name: p, prefer: [{geo: NL}, {geo: DE}, {provider: redshield}]}\n")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := cfg.Warnings()
	if len(got) != 1 || !strings.Contains(got[0], "prefer[2]") || !strings.Contains(got[0], "taken by prefer[0] {Geo:NL Provider:}, prefer[1] {Geo:DE Provider:}") {
		t.Errorf("Warnings = %q", got)
	}
}

func TestNoWarningsWhenEveryTierApplies(t *testing.T) {
	for _, prefer := range []string{
		"[]",
		"[{geo: NL}, {geo: DE}]",
		"[{provider: redshield}]",
		"[{geo: NL}, {provider: redshield}]",
		"[{}]",
	} {
		cfg, err := Parse([]byte(profilesConfig("  - {name: p, prefer: " + prefer + "}\n")))
		if err != nil {
			t.Fatalf("%s: %v", prefer, err)
		}
		if got := cfg.Warnings(); len(got) != 0 {
			t.Errorf("%s: Warnings = %q, want none", prefer, got)
		}
	}
}
