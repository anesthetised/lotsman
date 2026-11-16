// Package config loads and validates the operator-edited lotsman.yaml.
package config

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen    string       `yaml:"listen"`
	AWG       int          `yaml:"awg"` // AmneziaWG generation the listener speaks: 2 or 3
	Endpoint  string       `yaml:"endpoint"`
	Subnet    netip.Prefix `yaml:"subnet"`
	DNS       []netip.Addr `yaml:"dns"`
	StateDir  string       `yaml:"state_dir"`
	Upstreams []Upstream   `yaml:"upstreams"`
	Profiles  []Profile    `yaml:"profiles"`
	Health    Health       `yaml:"health"`
	Metrics   Metrics      `yaml:"metrics"`
}

// Metrics enables the Prometheus endpoint when Listen is set. The endpoint
// names users, so it must not be bound to an unspecified address.
type Metrics struct {
	Listen string `yaml:"listen"`
}

type Upstream struct {
	Name     string   `yaml:"name"`
	Conf     string   `yaml:"conf"`
	Geo      string   `yaml:"geo"`
	Provider string   `yaml:"provider"`
	Tags     []string `yaml:"tags"` // free-form groups a profile can prefer as one tier
}

func (u Upstream) HasTag(tag string) bool {
	return slices.Contains(u.Tags, tag)
}

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

// String lists the given fields the way the config spells them, e.g. {geo: NL, tag: eu},
// so warnings and `config check` stay short however many fields Match grows.
func (m Match) String() string {
	var parts []string
	for _, f := range [...]struct{ key, val string }{{"geo", m.Geo}, {"provider", m.Provider}, {"tag", m.Tag}} {
		if f.val != "" {
			parts = append(parts, f.key+": "+f.val)
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

type Health struct {
	Interval  time.Duration `yaml:"interval"`
	Probe     ProbeTargets  `yaml:"probe"`
	DownAfter int           `yaml:"down_after"`
	UpAfter   int           `yaml:"up_after"`
}

// ProbeTargets are the TCP destinations a health round tries through an
// upstream; the round passes when any one answers. In YAML it is a single
// "ip:port" or a list of them.
type ProbeTargets []netip.AddrPort

func (p *ProbeTargets) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		ap, err := netip.ParseAddrPort(n.Value)
		if err != nil {
			return fmt.Errorf("health.probe: %w", err)
		}
		*p = ProbeTargets{ap}
	case yaml.SequenceNode:
		targets := make(ProbeTargets, 0, len(n.Content))
		for i, item := range n.Content {
			ap, err := netip.ParseAddrPort(item.Value)
			if item.Kind != yaml.ScalarNode || err != nil {
				return fmt.Errorf("health.probe[%d]: want an ip:port, got %q", i, item.Value)
			}
			targets = append(targets, ap)
		}
		*p = targets
	default:
		return fmt.Errorf("health.probe: want an ip:port or a list of them")
	}
	return nil
}

func (p ProbeTargets) String() string {
	s := make([]string, len(p))
	for i, t := range p {
		s[i] = t.String()
	}
	return strings.Join(s, ", ")
}

func Default() Config {
	return Config{
		Listen:   "0.0.0.0:51820",
		AWG:      3,
		Subnet:   netip.MustParsePrefix("10.77.0.0/16"),
		DNS:      []netip.Addr{netip.MustParseAddr("1.1.1.1")},
		StateDir: "/var/lib/lotsman",
		Health:   Health{Interval: 15 * time.Second, Probe: ProbeTargets{netip.MustParseAddrPort("1.1.1.1:443")}, DownAfter: 3, UpAfter: 2},
	}
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func Parse(data []byte) (*Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var (
	profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	// An upstream name becomes the TUN interface "lm-up-<name>", which must fit IFNAMSIZ (15).
	upstreamNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,8}$`)
)

// InterfaceName is the TUN interface an upstream is bound to.
func (u Upstream) InterfaceName() string { return "lm-up-" + u.Name }

func (c *Config) Validate() error {
	if _, err := netip.ParseAddrPort(c.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if c.AWG != 2 && c.AWG != 3 {
		return fmt.Errorf("awg: must be 2 or 3, got %d", c.AWG)
	}
	host, port, err := net.SplitHostPort(c.Endpoint)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	if host == "" {
		return fmt.Errorf("endpoint: host is required")
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return fmt.Errorf("endpoint: port must be a number from 1 to 65535")
	}
	if !c.Subnet.Addr().Is4() || c.Subnet.Bits() > 24 {
		return fmt.Errorf("subnet: must be an IPv4 prefix of /24 or larger, got %s", c.Subnet)
	}
	if c.Subnet != c.Subnet.Masked() {
		return fmt.Errorf("subnet: host bits must be zero, got %s (use %s)", c.Subnet, c.Subnet.Masked())
	}
	if len(c.DNS) == 0 {
		return fmt.Errorf("dns: at least one resolver is required")
	}
	if c.StateDir == "" {
		return fmt.Errorf("state_dir: required")
	}
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("upstreams: at least one is required")
	}
	seen := map[string]bool{}
	for i, u := range c.Upstreams {
		if !upstreamNameRe.MatchString(u.Name) {
			return fmt.Errorf("upstreams[%d]: name %q must match %s (it becomes interface lm-up-<name>)", i, u.Name, upstreamNameRe)
		}
		if seen[u.Name] {
			return fmt.Errorf("upstreams[%d]: duplicate name %q", i, u.Name)
		}
		seen[u.Name] = true
		if u.Conf == "" {
			return fmt.Errorf("upstream %q: conf path is required", u.Name)
		}
		// geo and provider are not validated: a typo there surfaces as a
		// prefer entry matching nothing. Tags are checked the same way, but
		// an empty one would read as "any" in a matcher, and a duplicate is
		// a typo that cannot change what matches.
		for j, tag := range u.Tags {
			if !profileNameRe.MatchString(tag) {
				return fmt.Errorf("upstream %q: tag %q must match %s", u.Name, tag, profileNameRe)
			}
			if slices.Contains(u.Tags[:j], tag) {
				return fmt.Errorf("upstream %q: duplicate tag %q", u.Name, tag)
			}
		}
	}
	if len(c.Profiles) == 0 {
		return fmt.Errorf("profiles: at least one is required")
	}
	seen = map[string]bool{}
	for i, p := range c.Profiles {
		if !profileNameRe.MatchString(p.Name) {
			return fmt.Errorf("profiles[%d]: name %q must match %s", i, p.Name, profileNameRe)
		}
		if seen[p.Name] {
			return fmt.Errorf("profiles[%d]: duplicate name %q", i, p.Name)
		}
		seen[p.Name] = true
		// Typos in geo or provider must not pass silently, but an entry may
		// go empty when an upstream is removed; only a profile with no
		// usable tier at all is an error. Warnings reports the rest.
		if len(p.Prefer) > 0 && !slices.ContainsFunc(c.preferTiers(p), func(t preferTier) bool { return t.own > 0 }) {
			return fmt.Errorf("profile %q: prefer matches no upstream", p.Name)
		}
	}
	h := c.Health
	if h.Interval <= 0 {
		return fmt.Errorf("health.interval: must be positive")
	}
	if len(h.Probe) == 0 {
		return fmt.Errorf("health.probe: at least one target is required")
	}
	for i, t := range h.Probe {
		if !t.IsValid() {
			return fmt.Errorf("health.probe[%d]: invalid address", i)
		}
	}
	if h.DownAfter < 1 || h.UpAfter < 1 {
		return fmt.Errorf("health.down_after and health.up_after must be at least 1")
	}
	if c.Metrics.Listen != "" {
		ap, err := netip.ParseAddrPort(c.Metrics.Listen)
		if err != nil {
			return fmt.Errorf("metrics.listen: %w", err)
		}
		if ap.Addr().IsUnspecified() {
			return fmt.Errorf("metrics.listen: %s would expose user names to everyone; bind the tunnel or loopback address", ap.Addr())
		}
	}
	return nil
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

// Warnings lists problems that do not stop the configuration from loading: a
// prefer entry that matches no upstream, or whose upstreams all belong to
// earlier entries, is a tier that never applies. It expects a validated
// Config; the order is stable.
func (c *Config) Warnings() []string {
	var out []string
	for _, p := range c.Profiles {
		for j, t := range c.preferTiers(p) {
			m := p.Prefer[j]
			switch {
			case t.matched == 0:
				out = append(out, fmt.Sprintf("profile %q: prefer[%d] %v matches no upstream and is ignored", p.Name, j, m))
			case t.own == 0:
				var by []string
				for _, k := range t.shadowers {
					by = append(by, fmt.Sprintf("prefer[%d] %v", k, p.Prefer[k]))
				}
				out = append(out, fmt.Sprintf("profile %q: prefer[%d] %v never applies: every upstream it matches is taken by %s", p.Name, j, m, strings.Join(by, ", ")))
			}
		}
	}
	return out
}

func (c *Config) Profile(name string) (Profile, bool) {
	for _, p := range c.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}
