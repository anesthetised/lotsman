package config

import (
	"slices"
	"strings"
	"testing"
)

// profilesConfig is minimal with the given profiles in the legacy profiles:
// key, which check reads. The upstreams are ac-nl (NL, acme) and ac-de (DE, acme).
func profilesConfig(profiles string) string {
	return minimal + "profiles:\n" + profiles
}

// check validates the profiles of a legacy config against its upstreams, as
// the daemon and `config check` validate the stored ones.
func check(input string) ([]string, error) {
	cfg, profiles, err := ParseLegacy([]byte(input))
	if err != nil {
		return nil, err
	}
	return cfg.CheckProfiles(profiles)
}

// Removing the only NL upstream must not fail the reload of a profile that
// still has a DE tier; the emptied tier is only reported.
func TestPreferTierEmptiedByUpstreamRemovalIsWarning(t *testing.T) {
	input := profilesConfig("  - {name: eu, prefer: [{geo: NL}, {geo: DE}]}\n")
	input = strings.Replace(input, "  - name: ac-nl\n    conf: /etc/lotsman/upstreams/ac-nl.conf\n    geo: NL\n    provider: acme\n", "", 1)
	if strings.Contains(input, "ac-nl") {
		t.Fatal("test setup: ac-nl not removed")
	}
	got, err := check(input)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	want := []string{`profile "eu": tier 1 geo=NL matches no upstream and is ignored`}
	if !slices.Equal(got, want) {
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
			_, err := check(profilesConfig("  - {name: p, prefer: " + prefer + "}\n"))
			if err == nil || !strings.Contains(err.Error(), `profile "p": prefer matches no upstream`) {
				t.Fatalf("check error = %v", err)
			}
		})
	}
}

func TestShadowedPreferEntryIsWarningNamingShadower(t *testing.T) {
	// Every NL upstream is acme, so tier 2 has nothing left to take.
	got, err := check(profilesConfig("  - {name: p, prefer: [{provider: acme}, {geo: NL}]}\n"))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	want := []string{`profile "p": tier 2 geo=NL never applies: every upstream it matches is taken by tier 1 provider=acme`}
	if !slices.Equal(got, want) {
		t.Errorf("Warnings = %q, want %q", got, want)
	}
}

func TestShadowedPreferEntryNamesEveryShadower(t *testing.T) {
	got, err := check(profilesConfig("  - {name: p, prefer: [{geo: NL}, {geo: DE}, {provider: acme}]}\n"))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "tier 3 ") || !strings.Contains(got[0], "taken by tier 1 geo=NL, tier 2 geo=DE") {
		t.Errorf("Warnings = %q", got)
	}
}

func TestNoWarningsWhenEveryTierApplies(t *testing.T) {
	for _, prefer := range []string{
		"[]",
		"[{geo: NL}, {geo: DE}]",
		"[{provider: acme}]",
		"[{geo: NL}, {provider: acme}]",
		"[{}]",
	} {
		got, err := check(profilesConfig("  - {name: p, prefer: " + prefer + "}\n"))
		if err != nil {
			t.Fatalf("%s: %v", prefer, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: Warnings = %q, want none", prefer, got)
		}
	}
}

// tagsConfig has three upstreams: a (LT, near+eu), b (NL, eu) and c (US, no tags).
func tagsConfig(profiles string) string {
	return `
endpoint: vpn.example.org:51820
upstreams:
  - {name: a, conf: /a, geo: LT, tags: [near, eu]}
  - {name: b, conf: /b, geo: NL, tags: [eu]}
  - {name: c, conf: /c, geo: US}
profiles:
` + profiles
}

func TestTagOnlyProfileThatMatchesNothingIsError(t *testing.T) {
	for _, prefer := range []string{"[{tag: asia}]", "[{tag: asia}, {tag: Asia}]", "[{tag: near, geo: US}]"} {
		_, err := check(tagsConfig("  - {name: p, prefer: " + prefer + "}\n"))
		if err == nil || !strings.Contains(err.Error(), `profile "p": prefer matches no upstream`) {
			t.Errorf("%s: check error = %v", prefer, err)
		}
	}
}

func TestTagTierThatMatchesNothingIsWarning(t *testing.T) {
	got, err := check(tagsConfig("  - {name: p, prefer: [{tag: asia}, {tag: eu, geo: NL}]}\n"))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	want := []string{`profile "p": tier 1 tag=asia matches no upstream and is ignored`}
	if !slices.Equal(got, want) {
		t.Errorf("Warnings = %q, want %q", got, want)
	}
}

func TestShadowedTagTierIsWarning(t *testing.T) {
	// prefer[0] already takes every upstream that carries eu, so the later tiers need not apply.
	got, err := check(tagsConfig("  - {name: p, prefer: [{tag: eu}, {tag: near, geo: LT}, {tag: near}]}\n"))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	want := []string{
		`profile "p": tier 2 geo=LT,tag=near never applies: every upstream it matches is taken by tier 1 tag=eu`,
		`profile "p": tier 3 tag=near never applies: every upstream it matches is taken by tier 1 tag=eu`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("Warnings = %q, want %q", got, want)
	}
}

func TestNoWarningsWhenEveryTagTierApplies(t *testing.T) {
	got, err := check(tagsConfig("  - {name: p, prefer: [{tag: near}, {tag: eu}, {geo: US}]}\n"))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Warnings = %q, want none", got)
	}
}
