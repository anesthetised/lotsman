package config

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

const withOutline = `
endpoint: vpn.example.org:51820
upstreams:
  - {name: nl, conf: /etc/lotsman/upstreams/nl.conf, geo: NL, provider: acme}
  - {name: ol, kind: outline, conf: /etc/lotsman/upstreams/ol.key, geo: NL, provider: beta, max_flows: 100}
`

func TestUpstreamKinds(t *testing.T) {
	cfg, err := Parse([]byte(withOutline))
	if err != nil {
		t.Fatal(err)
	}
	nl, ol := cfg.Upstreams[0], cfg.Upstreams[1]
	if nl.Kind != KindAWG || nl.Proxy() {
		t.Errorf("default kind = %q, proxy %v", nl.Kind, nl.Proxy())
	}
	if ol.Kind != KindOutline || !ol.Proxy() || ol.MaxFlows != 100 {
		t.Errorf("outline upstream = %+v", ol)
	}
	vl, err := Parse([]byte(strings.Replace(withOutline, "kind: outline", "kind: vless", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if u := vl.Upstreams[1]; u.Kind != KindVLESS || !u.Proxy() || u.MaxFlows != 100 {
		t.Errorf("vless upstream = %+v", u)
	}
	if want := (DNSTargets{netip.MustParseAddrPort("1.1.1.1:53")}); !slices.Equal(cfg.Health.DNS, want) {
		t.Errorf("health.dns default = %v, want %v", cfg.Health.DNS, want)
	}

	cfg, err = Parse([]byte(withOutline + "health: {dns: ['9.9.9.9:53', '[2620:fe::fe]:53']}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Health.DNS.String(); got != "9.9.9.9:53, [2620:fe::fe]:53" {
		t.Errorf("health.dns = %s", got)
	}
}

func TestUpstreamKindErrors(t *testing.T) {
	for name, tc := range map[string]struct{ config, want string }{
		"unknown kind":              {strings.Replace(withOutline, "kind: outline", "kind: trojan", 1), `kind must be awg, outline or vless, got "trojan"`},
		"max_flows on awg":          {strings.Replace(withOutline, "nl.conf, geo: NL, provider: acme}", "nl.conf, max_flows: 5}", 1), "max_flows applies to proxy upstreams only"},
		"negative max_flows":        {strings.Replace(withOutline, "max_flows: 100", "max_flows: -1", 1), "max_flows must not be negative"},
		"no dns targets":            {withOutline + "health: {dns: []}\n", "health.dns: at least one resolver"},
		"bad dns target":            {withOutline + "health: {dns: [1.1.1.1]}\n", "health.dns[0]:"},
		"dns mapping":               {withOutline + "health: {dns: {a: 1}}\n", "health.dns:"},
		"subnet with proxy address": {withOutline + "subnet: 192.0.0.0/24\n", "192.0.0.8, the address of proxy upstreams"},
		"vless without dns":         {strings.Replace(withOutline, "kind: outline", "kind: vless", 1) + "health: {dns: []}\n", "health.dns: at least one resolver"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.config)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want error containing %q", err, tc.want)
			}
		})
	}
	// Without proxy upstreams neither the resolvers nor the address matter.
	if _, err := Parse([]byte(minimal + "subnet: 192.0.0.0/24\nhealth: {dns: []}\n")); err != nil {
		t.Errorf("AWG-only config: %v", err)
	}
}
