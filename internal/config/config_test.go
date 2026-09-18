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
	cases := map[string]string{
		"unknown field":          "bogus: 1\n",
		"missing endpoint":       "endpoint: ''\n",
		"endpoint without port":  "endpoint: vpn.example.org\n",
		"ipv6 subnet":            "subnet: fd00::/64\n",
		"subnet too small":       "subnet: 10.0.0.0/30\n",
		"empty dns":              "dns: []\n",
		"bad upstream name":      "upstreams: [{name: 'Bad Name', conf: /x}]\n",
		"upstream name too long": "upstreams: [{name: redshield-nl, conf: /x}]\nprofiles: [{name: p}]\n",
		"duplicate upstream":     "upstreams: [{name: a, conf: /x}, {name: a, conf: /y}]\nprofiles: [{name: p}]\n",
		"upstream without conf":  "upstreams: [{name: a}]\n",
		"no profiles":            "profiles: []\n",
		"duplicate profile":      "profiles: [{name: p}, {name: p}]\n",
		"prefer matches nothing": "profiles: [{name: p, prefer: [{geo: XX}]}]\n",
		"zero health interval":   "health: {interval: 0s}\n",
		"probe without port":     "health: {probe: 1.1.1.1}\n",
		"down_after zero":        "health: {down_after: 0}\n",
		"metrics without port":   "metrics: {listen: 10.77.0.1}\n",
		"metrics on any address": "metrics: {listen: 0.0.0.0:9100}\n",
		"metrics on any v6":      "metrics: {listen: '[::]:9100'}\n",
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(minimal + override)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
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
