package config

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

const minimal = `
endpoint: vpn.example.org:51820
upstreams:
  - name: ac-nl
    conf: /etc/lotsman/upstreams/ac-nl.conf
    geo: NL
    provider: acme
  - name: ac-de
    conf: /etc/lotsman/upstreams/ac-de.conf
    geo: DE
    provider: acme
`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:51820" {
		t.Errorf("listen default = %q", cfg.Listen)
	}
	if cfg.AWG != 3 {
		t.Errorf("awg default = %d, want 3", cfg.AWG)
	}
	if cfg.Subnet != netip.MustParsePrefix("10.77.0.0/16") {
		t.Errorf("subnet default = %s", cfg.Subnet)
	}
	if cfg.Health.Interval != 15*time.Second || cfg.Health.DownAfter != 3 || cfg.Health.UpAfter != 2 {
		t.Errorf("health defaults = %+v", cfg.Health)
	}
}

func TestParseOverrides(t *testing.T) {
	cfg, err := Parse([]byte(minimal + `
listen: 127.0.0.1:1234
awg: 2
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
	if cfg.Listen != "127.0.0.1:1234" || cfg.AWG != 2 || cfg.Subnet.String() != "10.9.0.0/24" || len(cfg.DNS) != 2 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	h := cfg.Health
	if h.Interval != time.Minute || h.DownAfter != 5 || h.UpAfter != 1 || !slices.Equal(h.Probe, ProbeTargets{netip.MustParseAddrPort("8.8.8.8:53")}) {
		t.Errorf("health = %+v", h)
	}
}

func TestProbeAcceptsOneTargetOrAList(t *testing.T) {
	a, b := netip.MustParseAddrPort("1.1.1.1:443"), netip.MustParseAddrPort("[2606:4700::1111]:443")
	for name, tc := range map[string]struct {
		health string
		want   ProbeTargets
	}{
		"default":           {"", ProbeTargets{a}},
		"single string":     {"health: {probe: 1.1.1.1:443}\n", ProbeTargets{a}},
		"flow list":         {"health: {probe: ['1.1.1.1:443', '[2606:4700::1111]:443']}\n", ProbeTargets{a, b}},
		"block list":        {"health:\n  probe:\n    - 1.1.1.1:443\n    - '[2606:4700::1111]:443'\n", ProbeTargets{a, b}},
		"one-item list":     {"health: {probe: [1.1.1.1:443]}\n", ProbeTargets{a}},
		"other fields kept": {"health: {interval: 1m}\n", ProbeTargets{a}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]byte(minimal + tc.health))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.Health.Probe, tc.want) {
				t.Errorf("probe = %v, want %v", cfg.Health.Probe, tc.want)
			}
		})
	}
}

func TestProbeRejectsBadTargets(t *testing.T) {
	for name, tc := range map[string]struct{ health, want string }{
		"empty list":     {"health: {probe: []}\n", "health.probe: at least one target"},
		"null":           {"health: {probe: null}\n", "health.probe: at least one target"},
		"empty string":   {"health: {probe: ''}\n", "health.probe:"},
		"bad list entry": {"health: {probe: [1.1.1.1:443, 8.8.8.8]}\n", "health.probe[1]:"},
		"hostname entry": {"health: {probe: [one.one.one.one:443]}\n", "health.probe[0]:"},
		"nested entry":   {"health: {probe: [[1.1.1.1:443]]}\n", "health.probe[0]:"},
		"mapping":        {"health: {probe: {addr: 1.1.1.1:443}}\n", "health.probe:"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(minimal + tc.health)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateRejectsEmptyProbeTargets(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Health.Probe = nil
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "health.probe: at least one target") {
		t.Errorf("Validate error = %v", err)
	}
	cfg.Health.Probe = ProbeTargets{{}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "health.probe[0]:") {
		t.Errorf("Validate error = %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]struct {
		field string
		value string
		want  string
	}{
		"unknown field":          {"bogus", "bogus: 1\n", "field bogus not found"},
		"awg zero":               {"awg", "awg: 0\n", "awg: must be 2 or 3, got 0"},
		"awg one":                {"awg", "awg: 1\n", "awg: must be 2 or 3, got 1"},
		"awg four":               {"awg", "awg: 4\n", "awg: must be 2 or 3, got 4"},
		"awg not a number":       {"awg", "awg: three\n", "cannot unmarshal"},
		"missing endpoint":       {"endpoint", "endpoint: ''\n", "endpoint:"},
		"endpoint without port":  {"endpoint", "endpoint: vpn.example.org\n", "endpoint:"},
		"endpoint empty host":    {"endpoint", "endpoint: ':51820'\n", "endpoint: host is required"},
		"endpoint service port":  {"endpoint", "endpoint: vpn.example.org:banana\n", "endpoint: port must be a number from 1 to 65535"},
		"endpoint empty port":    {"endpoint", "endpoint: 'vpn.example.org:'\n", "endpoint: port must be a number from 1 to 65535"},
		"endpoint zero port":     {"endpoint", "endpoint: vpn.example.org:0\n", "endpoint: port must be a number from 1 to 65535"},
		"endpoint port overflow": {"endpoint", "endpoint: vpn.example.org:65536\n", "endpoint: port must be a number from 1 to 65535"},
		"endpoint negative port": {"endpoint", "endpoint: vpn.example.org:-1\n", "endpoint: port must be a number from 1 to 65535"},
		"endpoint signed port":   {"endpoint", "endpoint: vpn.example.org:+1\n", "endpoint: port must be a number from 1 to 65535"},
		"ipv6 subnet":            {"subnet", "subnet: fd00::/64\n", "subnet:"},
		"subnet too small":       {"subnet", "subnet: 10.0.0.0/30\n", "subnet:"},
		"empty dns":              {"dns", "dns: []\n", "dns:"},
		"bad upstream name":      {"upstreams", "upstreams: [{name: 'Bad Name', conf: /x}]\n", "upstreams[0]: name"},
		"upstream name too long": {"upstreams", "upstreams: [{name: acmecorp-nl, conf: /x}]\n", "upstreams[0]: name"},
		"duplicate upstream":     {"upstreams", "upstreams: [{name: a, conf: /x, geo: NL, provider: acme}, {name: a, conf: /y}]\n", "duplicate name"},
		"upstream without conf":  {"upstreams", "upstreams: [{name: a}]\n", "conf path is required"},
		"upstream without geo":   {"upstreams", "upstreams: [{name: a, conf: /x, provider: acme}]\n", `upstream "a": geo "" must be an ISO 3166-1 alpha-2`},
		"lowercase geo":          {"upstreams", "upstreams: [{name: a, conf: /x, geo: nl, provider: acme}]\n", `geo "nl" must be`},
		"three-letter geo":       {"upstreams", "upstreams: [{name: a, conf: /x, geo: NLD, provider: acme}]\n", `geo "NLD" must be`},
		"upstream w/o provider":  {"upstreams", "upstreams: [{name: a, conf: /x, geo: NL}]\n", `upstream "a": provider "" must match`},
		"bad provider":           {"upstreams", "upstreams: [{name: a, conf: /x, geo: NL, provider: Acme Corp}]\n", `provider "Acme Corp" must match`},
		"provider too long":      {"upstreams", "upstreams: [{name: a, conf: /x, geo: NL, provider: " + strings.Repeat("a", 33) + "}]\n", "must match"},
		"tags are gone":          {"upstreams", "upstreams: [{name: a, conf: /x, geo: NL, provider: acme, tags: [eu]}]\n", "field tags not found"},
		"profiles are gone":      {"profiles", "profiles: [{name: p}]\n", "field profiles not found"},
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

func TestEndpointValidForms(t *testing.T) {
	for _, endpoint := range []string{"vpn.example.org:1", "vpn.example.org:65535", "192.0.2.1:51820", "[2001:db8::1]:51820"} {
		t.Run(endpoint, func(t *testing.T) {
			input := replaceTopLevelField(minimal, "endpoint", "endpoint: '"+endpoint+"'\n")
			cfg, err := Parse([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Endpoint != endpoint {
				t.Errorf("endpoint changed to %q, want %q", cfg.Endpoint, endpoint)
			}
		})
	}
}

func TestValidateRejectsSubnetHostBits(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	for _, subnet := range []string{"10.77.0.1/24", "10.77.0.255/24", "10.77.0.255/16", "0.0.0.1/0"} {
		t.Run(subnet, func(t *testing.T) {
			cfg.Subnet = netip.MustParsePrefix(subnet)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "subnet: host bits must be zero") {
				t.Errorf("Validate error = %v, want subnet host-bits error", err)
			}
			if cfg.Subnet.String() != subnet {
				t.Errorf("Validate changed subnet to %s", cfg.Subnet)
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

// The shipped example is the first thing an operator copies; it must load.
func TestExampleConfigIsValid(t *testing.T) {
	if _, err := Load("../../deploy/lotsman.example.yaml"); err != nil {
		t.Fatal(err)
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

func TestUsersMaxDevices(t *testing.T) {
	for _, tc := range []struct {
		extra string
		want  int
		ok    bool
	}{
		{"", DefaultMaxDevices, true},
		{"users: {max_devices: 2}\n", 2, true},
		{"users: {max_devices: 0}\n", 0, true}, // users add nothing
		{"users: {max_devices: -1}\n", 0, false},
	} {
		cfg, err := Parse([]byte(minimal + tc.extra))
		if (err == nil) != tc.ok || (tc.ok && cfg.Users.MaxDevices != tc.want) {
			t.Errorf("%q: %+v, %v; want %d", tc.extra, cfg, err, tc.want)
		}
	}
}

func TestInterfaceName(t *testing.T) {
	if got := (Upstream{Name: "ac-nl"}).InterfaceName(); got != "lm-up-ac-nl" || len(got) > 15 {
		t.Errorf("InterfaceName = %q", got)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/lotsman.yaml"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("expected file error, got %v", err)
	}
}

func TestTransfer(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil || cfg.Health.Transfer != nil {
		t.Fatalf("transfer is off unless configured: %+v, %v", cfg.Health.Transfer, err)
	}
	for name, tc := range map[string]struct {
		health string
		want   Transfer
	}{
		"one url, defaults": {"health: {transfer: {url: 'https://speed.example/down?bytes=65536'}}\n",
			Transfer{URL: TransferURLs{"https://speed.example/down?bytes=65536"}, MinBytes: 65536, UploadBytes: 256 << 10, Timeout: 10 * time.Second, Interval: 5 * time.Minute}},
		"list and overrides": {"health:\n  transfer:\n    url: ['http://a.example/x', 'https://b.example:8443/y']\n    min_bytes: 1000\n    upload_url: https://speed.example/up\n    upload_bytes: 2000\n    timeout: 3s\n    interval: 1m\n",
			Transfer{URL: TransferURLs{"http://a.example/x", "https://b.example:8443/y"}, MinBytes: 1000, UploadURL: "https://speed.example/up", UploadBytes: 2000, Timeout: 3 * time.Second, Interval: time.Minute}},
		"null block is off": {"health: {transfer: null}\n", Transfer{}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]byte(minimal + tc.health))
			if err != nil {
				t.Fatal(err)
			}
			var got Transfer
			if cfg.Health.Transfer != nil {
				got = *cfg.Health.Transfer
			}
			if !slices.Equal(got.URL, tc.want.URL) || got.MinBytes != tc.want.MinBytes || got.UploadURL != tc.want.UploadURL ||
				got.UploadBytes != tc.want.UploadBytes || got.Timeout != tc.want.Timeout || got.Interval != tc.want.Interval {
				t.Errorf("transfer = %+v, want %+v", got, tc.want)
			}
		})
	}
	for name, tc := range map[string]struct{ health, want string }{
		"empty block":      {"health: {transfer: {}}\n", "health.transfer.url: at least one URL"},
		"empty list":       {"health: {transfer: {url: []}}\n", "health.transfer.url: at least one URL"},
		"no scheme":        {"health: {transfer: {url: speed.example/down}}\n", "health.transfer.url[0]:"},
		"other scheme":     {"health: {transfer: {url: 'ftp://speed.example/down'}}\n", "health.transfer.url[0]:"},
		"no host":          {"health: {transfer: {url: ['http://a.example/', 'https:///down']}}\n", "health.transfer.url[1]:"},
		"mapping":          {"health: {transfer: {url: {u: 'http://a.example/'}}}\n", "health.transfer.url:"},
		"negative bytes":   {"health: {transfer: {url: 'http://a.example/', min_bytes: -1}}\n", "health.transfer.min_bytes"},
		"negative timeout": {"health: {transfer: {url: 'http://a.example/', timeout: -1s}}\n", "health.transfer.timeout"},
		"unknown key":      {"health: {transfer: {url: 'http://a.example/', bytes: 1}}\n", "field bytes not found"},
		"bad upload url":   {"health: {transfer: {url: 'http://a.example/', upload_url: 'ftp://a.example/'}}\n", "health.transfer.upload_url:"},
		"negative upload":  {"health: {transfer: {url: 'http://a.example/', upload_bytes: -1}}\n", "health.transfer.upload_bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(minimal + tc.health)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSpeed(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil || cfg.Health.Speed != nil {
		t.Fatalf("speed tests are off unless configured: %+v, %v", cfg.Health.Speed, err)
	}
	urls := "health: {speed: {down_url: 'https://speed.example/down?bytes=2000000', up_url: 'https://speed.example/up'"
	cfg, err = Parse([]byte(minimal + urls + "}}\n"))
	want := Speed{DownURL: "https://speed.example/down?bytes=2000000", UpURL: "https://speed.example/up",
		Bytes: 2_000_000, Interval: 30 * time.Minute, Timeout: 30 * time.Second}
	if err != nil || *cfg.Health.Speed != want {
		t.Errorf("defaults: %+v, %v; want %+v", cfg.Health.Speed, err, want)
	}
	cfg, err = Parse([]byte(minimal + urls + ", bytes: 1000, interval: 1h, timeout: 5s}}\n"))
	want.Bytes, want.Interval, want.Timeout = 1000, time.Hour, 5*time.Second
	if err != nil || *cfg.Health.Speed != want {
		t.Errorf("overrides: %+v, %v; want %+v", cfg.Health.Speed, err, want)
	}
	for name, tc := range map[string]struct{ health, want string }{
		"no down url":      {"health: {speed: {up_url: 'https://a.example/'}}\n", "health.speed.down_url"},
		"no up url":        {"health: {speed: {down_url: 'https://a.example/'}}\n", "health.speed.up_url"},
		"other scheme":     {"health: {speed: {down_url: 'ftp://a.example/', up_url: 'https://a.example/'}}\n", "health.speed.down_url"},
		"negative bytes":   {urls + ", bytes: -1}}\n", "health.speed.bytes"},
		"negative timeout": {urls + ", timeout: -1s}}\n", "health.speed.timeout"},
		"unknown key":      {urls + ", url: 'https://a.example/'}}\n", "field url not found"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(minimal + tc.health)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestIncidents(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil || cfg.Incidents.SendErrorsPerMinute != 5 {
		t.Fatalf("default = %+v, %v", cfg.Incidents, err)
	}
	// Zero is a threshold too: any error opens an incident.
	cfg, err = Parse([]byte(minimal + "incidents: {send_errors_per_minute: 0}\n"))
	if err != nil || cfg.Incidents.SendErrorsPerMinute != 0 {
		t.Errorf("zero = %+v, %v", cfg.Incidents, err)
	}
	if _, err := Parse([]byte(minimal + "incidents: {send_errors_per_minute: -1}\n")); err == nil || !strings.Contains(err.Error(), "incidents.send_errors_per_minute") {
		t.Errorf("negative: %v", err)
	}
}

func TestHistoryRetention(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil || cfg.History.MinuteRetention != 7*24*time.Hour || cfg.History.HourRetention != 90*24*time.Hour {
		t.Fatalf("defaults = %+v, %v", cfg.History, err)
	}
	// One key keeps the other's default.
	cfg, err = Parse([]byte(minimal + "history: {minute_retention: 48h}\n"))
	if err != nil || cfg.History.MinuteRetention != 48*time.Hour || cfg.History.HourRetention != 90*24*time.Hour {
		t.Errorf("minute_retention only = %+v, %v", cfg.History, err)
	}
	for name, yaml := range map[string]string{
		"history.minute_retention": "history: {minute_retention: 30m}\n",
		"history.hour_retention":   "history: {minute_retention: 240h, hour_retention: 24h}\n",
	} {
		if _, err := Parse([]byte(minimal + yaml)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", yaml, err)
		}
	}
}
