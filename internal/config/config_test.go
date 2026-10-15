package config

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

const minimal = `
endpoint: vpn.example.org:51820
upstreams:
  - name: rs-nl
    conf: /etc/lotsman/upstreams/rs-nl.conf
    geo: NL
    provider: redshield
  - name: rs-de
    conf: /etc/lotsman/upstreams/rs-de.conf
    geo: DE
    provider: redshield
profiles:
  - name: nl
    prefer: [{geo: NL}]
  - name: auto
    prefer: []
`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:51820" {
		t.Errorf("listen default = %q", cfg.Listen)
	}
	if cfg.Subnet != netip.MustParsePrefix("10.77.0.0/16") {
		t.Errorf("subnet default = %s", cfg.Subnet)
	}
	if cfg.Health.Interval != 15*time.Second || cfg.Health.DownAfter != 3 || cfg.Health.UpAfter != 2 {
		t.Errorf("health defaults = %+v", cfg.Health)
	}
	if p, ok := cfg.Profile("nl"); !ok || len(p.Prefer) != 1 || p.Prefer[0].Geo != "NL" {
		t.Errorf("profile nl = %+v, %v", p, ok)
	}
}

func TestParseOverrides(t *testing.T) {
	cfg, err := Parse([]byte(minimal + `
listen: 127.0.0.1:1234
subnet: 10.9.0.0/24
dns: [9.9.9.9, 149.112.112.112]
state_dir: /tmp/lotsman
health:
  interval: 1m
  probe: 8.8.8.8:53
  down_after: 5
  up_after: 1
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:1234" || cfg.Subnet.String() != "10.9.0.0/24" || len(cfg.DNS) != 2 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.Health != (Health{Interval: time.Minute, Probe: "8.8.8.8:53", DownAfter: 5, UpAfter: 1}) {
		t.Errorf("health = %+v", cfg.Health)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]struct {
		field string
		value string
		want  string
	}{
		"unknown field":          {"bogus", "bogus: 1\n", "field bogus not found"},
		"missing endpoint":       {"endpoint", "endpoint: ''\n", "endpoint:"},
		"endpoint without port":  {"endpoint", "endpoint: vpn.example.org\n", "endpoint:"},
		"ipv6 subnet":            {"subnet", "subnet: fd00::/64\n", "subnet:"},
		"subnet too small":       {"subnet", "subnet: 10.0.0.0/30\n", "subnet:"},
		"empty dns":              {"dns", "dns: []\n", "dns:"},
		"bad upstream name":      {"upstreams", "upstreams: [{name: 'Bad Name', conf: /x}]\n", "upstreams[0]: name"},
		"upstream name too long": {"upstreams", "upstreams: [{name: redshield-nl, conf: /x}]\n", "upstreams[0]: name"},
		"duplicate upstream":     {"upstreams", "upstreams: [{name: a, conf: /x}, {name: a, conf: /y}]\n", "duplicate name"},
		"upstream without conf":  {"upstreams", "upstreams: [{name: a}]\n", "conf path is required"},
		"no profiles":            {"profiles", "profiles: []\n", "profiles: at least one"},
		"bad profile name":       {"profiles", "profiles: [{name: 'Bad Name'}]\n", "profiles[0]: name"},
		"duplicate profile":      {"profiles", "profiles: [{name: p}, {name: p}]\n", "duplicate name"},
		"prefer matches nothing": {"profiles", "profiles: [{name: p, prefer: [{geo: XX}]}]\n", "matches no upstream"},
		"zero health interval":   {"health", "health: {interval: 0s}\n", "health.interval:"},
		"probe without port":     {"health", "health: {probe: 1.1.1.1}\n", "health.probe:"},
		"down_after zero":        {"health", "health: {down_after: 0}\n", "health.down_after"},
		"metrics without port":   {"metrics", "metrics: {listen: 10.77.0.1}\n", "metrics.listen:"},
		"metrics on any address": {"metrics", "metrics: {listen: 0.0.0.0:9100}\n", "would expose user names"},
		"metrics on any v6":      {"metrics", "metrics: {listen: '[::]:9100'}\n", "would expose user names"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			input := replaceTopLevelField(minimal, tc.field, tc.value)
			if _, err := Parse([]byte(input)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func replaceTopLevelField(yaml, field, replacement string) string {
	lines := strings.Split(yaml, "\n")
	start, end := -1, len(lines)
	for i, line := range lines {
		if strings.HasPrefix(line, field+":") {
			start = i
			for j := i + 1; j < len(lines); j++ {
				if lines[j] != "" && !strings.HasPrefix(lines[j], " ") {
					end = j
					break
				}
			}
			break
		}
	}
	if start == -1 {
		return yaml + replacement
	}
	return strings.Join(lines[:start], "\n") + "\n" + replacement + strings.Join(lines[end:], "\n")
}

func TestMetricsListen(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil || cfg.Metrics.Listen != "" {
		t.Fatalf("metrics should be off by default: %+v, %v", cfg.Metrics, err)
	}
	cfg, err = Parse([]byte(minimal + "metrics: {listen: 10.77.0.1:9100}\n"))
	if err != nil || cfg.Metrics.Listen != "10.77.0.1:9100" {
		t.Errorf("metrics.listen = %q, %v", cfg.Metrics.Listen, err)
	}
}

func TestInterfaceName(t *testing.T) {
	if got := (Upstream{Name: "rs-nl"}).InterfaceName(); got != "lm-up-rs-nl" || len(got) > 15 {
		t.Errorf("InterfaceName = %q", got)
	}
}

func TestMatch(t *testing.T) {
	u := Upstream{Name: "x", Geo: "NL", Provider: "rs"}
	for m, want := range map[Match]bool{
		{}:                             true,
		{Geo: "NL"}:                    true,
		{Provider: "rs"}:               true,
		{Geo: "NL", Provider: "rs"}:    true,
		{Geo: "DE"}:                    false,
		{Geo: "NL", Provider: "other"}: false,
	} {
		if got := m.Matches(u); got != want {
			t.Errorf("%+v.Matches = %v, want %v", m, got, want)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/lotsman.yaml"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("expected file error, got %v", err)
	}
}
