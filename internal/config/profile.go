package config

import (
	"fmt"
	"slices"
	"strings"
)

// Profile is what a peer's traffic prefers. Profiles live in the store; this
// package validates them against the upstreams of a config.
type Profile struct {
	Name   string  `yaml:"name"`
	Prefer []Match `yaml:"prefer"`
}

// Match selects upstreams. Empty fields match anything, so an empty Match matches all;
// when several fields are given, all of them must match.
type Match struct {
	Geo      string `yaml:"geo"`
	Provider string `yaml:"provider"`
	Tag      string `yaml:"tag"`
}

func (m Match) Matches(u Upstream) bool {
	return (m.Geo == "" || m.Geo == u.Geo) && (m.Provider == "" || m.Provider == u.Provider) && (m.Tag == "" || u.HasTag(m.Tag))
}

// String writes m as the CLI takes a tier: the given fields as key=value
// pairs joined by commas, e.g. geo=NL,tag=eu, or any when none is given.
func (m Match) String() string {
	var parts []string
	for _, f := range [...]struct{ key, val string }{{"geo", m.Geo}, {"provider", m.Provider}, {"tag", m.Tag}} {
		if f.val != "" {
			parts = append(parts, f.key+"="+f.val)
		}
	}
	if len(parts) == 0 {
		return "any"
	}
	return strings.Join(parts, ",")
}

// ValidateProfile checks p against the upstreams: its name, and that its
// prefer tiers leave at least one upstream to use. Typos in geo or provider
// must not pass silently, but a tier may go empty when an upstream is
// removed; only a profile with no usable tier at all is an error. Warnings
// reports the rest.
func (c *Config) ValidateProfile(p Profile) error {
	if !profileNameRe.MatchString(p.Name) {
		return fmt.Errorf("profile name %q must match %s", p.Name, profileNameRe)
	}
	if len(p.Prefer) > 0 && !slices.ContainsFunc(c.preferTiers(p), func(t preferTier) bool { return t.own > 0 }) {
		return fmt.Errorf("profile %q: prefer matches no upstream", p.Name)
	}
	return nil
}

// CheckProfiles validates every profile, as the daemon does on reload, and
// returns their warnings.
func (c *Config) CheckProfiles(profiles []Profile) ([]string, error) {
	for _, p := range profiles {
		if err := c.ValidateProfile(p); err != nil {
			return nil, err
		}
	}
	return c.Warnings(profiles), nil
}

// preferTier describes one prefer entry of a profile. Upstreams belong to the
// first entry that matches them, as in policy.Select.
type preferTier struct {
	matched   int   // upstreams the entry matches, including ones an earlier entry took
	own       int   // upstreams for which it is the first match; the tier's size
	shadowers []int // earlier entries that took upstreams this entry also matches
}

func (c *Config) preferTiers(p Profile) []preferTier {
	tiers := make([]preferTier, len(p.Prefer))
	for _, u := range c.Upstreams {
		first := -1
		for j, m := range p.Prefer {
			if !m.Matches(u) {
				continue
			}
			tiers[j].matched++
			switch {
			case first < 0:
				first = j
				tiers[j].own++
			case !slices.Contains(tiers[j].shadowers, first):
				tiers[j].shadowers = append(tiers[j].shadowers, first)
			}
		}
	}
	for j := range tiers {
		slices.Sort(tiers[j].shadowers)
	}
	return tiers
}

// Warnings lists problems that do not stop the profiles from being used: a
// prefer entry that matches no upstream, or whose upstreams all belong to
// earlier entries, is a tier that never applies. The order is stable.
func (c *Config) Warnings(profiles []Profile) []string {
	var out []string
	for _, p := range profiles {
		for j, t := range c.preferTiers(p) {
			m := p.Prefer[j]
			switch {
			case t.matched == 0:
				out = append(out, fmt.Sprintf("profile %q: tier %d %v matches no upstream and is ignored", p.Name, j+1, m))
			case t.own == 0:
				var by []string
				for _, k := range t.shadowers {
					by = append(by, fmt.Sprintf("tier %d %v", k+1, p.Prefer[k]))
				}
				out = append(out, fmt.Sprintf("profile %q: tier %d %v never applies: every upstream it matches is taken by %s", p.Name, j+1, m, strings.Join(by, ", ")))
			}
		}
	}
	return out
}
