// Package config loads and validates the operator-edited lotsman.yaml.
package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen    string       `yaml:"listen"`
	AWG       int          `yaml:"awg"`      // AmneziaWG generation the listener speaks: 2 or 3
	Endpoint  string       `yaml:"endpoint"` // IP:port that client configs connect to; never a hostname
	Subnet    netip.Prefix `yaml:"subnet"`
	DNS       []netip.Addr `yaml:"dns"`
	StateDir  string       `yaml:"state_dir"`
	Upstreams []Upstream   `yaml:"upstreams"`
	Health    Health       `yaml:"health"`
	Metrics   Metrics      `yaml:"metrics"`
	History   History      `yaml:"history"`
	Users     Users        `yaml:"users"`
}

// History is how long the availability and traffic history is kept: samples
// of a minute for MinuteRetention, then samples of an hour until HourRetention
// has passed since the sample, counted from now.
type History struct {
	MinuteRetention time.Duration `yaml:"minute_retention"`
	HourRetention   time.Duration `yaml:"hour_retention"`
}

// Users limits what users may do on their own page in the web panel. The
// operator is not limited.
type Users struct {
	// MaxDevices is how many devices a user may have when adding one on
	// their page; 0 turns adding off.
	MaxDevices int `yaml:"max_devices"`
}

// DefaultMaxDevices is users.max_devices when the config does not set it.
const DefaultMaxDevices = 5

// Metrics enables the Prometheus endpoint when Listen is set. The endpoint
// names users, so it must not be bound to an unspecified address.
type Metrics struct {
	Listen string `yaml:"listen"`
}

// Upstream kinds. An AWG upstream is an AmneziaWG (or WireGuard) tunnel.
// The others are proxy upstreams, a proxy behind a userspace stack: an
// Outline upstream is a Shadowsocks proxy, a VLESS upstream a VLESS server
// with REALITY and XTLS Vision.
const (
	KindAWG     = "awg"
	KindOutline = "outline"
	KindVLESS   = "vless"
)

// ProxyAddress is the address of every proxy upstream's TUN: the source of
// its health probes and, after masquerading, of its clients' flows. It is
// the IPv4 dummy address (RFC 7600), reserved for hosts that need a source
// address and have none. Sharing it is fine, as it is for AWG providers
// that hand out the same address: probe routing selects by interface too.
var ProxyAddress = netip.MustParsePrefix("192.0.0.8/32")

type Upstream struct {
	Name     string `yaml:"name"`
	Kind     string `yaml:"kind"`     // KindAWG (the default), KindOutline or KindVLESS
	Conf     string `yaml:"conf"`     // the provider's AmneziaWG config, or a file with the access key
	Geo      string `yaml:"geo"`      // the exit's country, ISO 3166-1 alpha-2
	Provider string `yaml:"provider"` // who runs the exit; a route may name it
	// MaxFlows caps the TCP and UDP flows a proxy upstream carries at once;
	// 0 is the default.
	MaxFlows int `yaml:"max_flows"`
}

// Proxy reports whether the upstream runs on the userspace proxy stack.
func (u Upstream) Proxy() bool { return u.Kind == KindOutline || u.Kind == KindVLESS }

type Health struct {
	Interval  time.Duration `yaml:"interval"`
	Probe     ProbeTargets  `yaml:"probe"`
	DNS       DNSTargets    `yaml:"dns"`
	DownAfter int           `yaml:"down_after"`
	UpAfter   int           `yaml:"up_after"`
	// Transfer, when set, also downloads through every upstream now and
	// then; nil (the block is absent) turns it off.
	Transfer *Transfer `yaml:"transfer"`
}

// Transfer is the throughput probe: an upstream that passes its
// reachability probes but cannot move MinBytes of a download within Timeout
// is degraded. It runs every Interval, give or take 20%, per upstream.
type Transfer struct {
	URL      TransferURLs  `yaml:"url"`
	MinBytes int64         `yaml:"min_bytes"`
	Timeout  time.Duration `yaml:"timeout"`
	Interval time.Duration `yaml:"interval"`
}

// Transfer defaults, applied by Parse to fields left out or zero.
const (
	DefaultTransferMinBytes = 64 << 10
	DefaultTransferTimeout  = 10 * time.Second
	DefaultTransferInterval = 5 * time.Minute
)

// TransferURLs are the http:// or https:// URLs a transfer probe downloads;
// a probe tries them in random order until one passes. In YAML it is a
// single URL or a list of them.
type TransferURLs []string

func (u *TransferURLs) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*u = TransferURLs{n.Value}
		return nil
	case yaml.SequenceNode:
		var urls []string
		if err := n.Decode(&urls); err != nil {
			return fmt.Errorf("health.transfer.url: want a URL or a list of them")
		}
		*u = urls
		return nil
	}
	return fmt.Errorf("health.transfer.url: want a URL or a list of them")
}

func (u TransferURLs) String() string { return strings.Join(u, ", ") }

// ProbeTargets are the TCP destinations a health round tries through an AWG
// upstream; the round passes when any one answers. In YAML it is a single
// "ip:port" or a list of them.
type ProbeTargets []netip.AddrPort

// DNSTargets are the resolvers a health round queries through a proxy
// upstream, over UDP and over TCP; the round passes when both protocols
// get an answer, from any target. The YAML form is that of ProbeTargets.
type DNSTargets []netip.AddrPort

func (p *ProbeTargets) UnmarshalYAML(n *yaml.Node) error {
	targets, err := parseTargets(n, "health.probe")
	*p = targets
	return err
}

func (p *DNSTargets) UnmarshalYAML(n *yaml.Node) error {
	targets, err := parseTargets(n, "health.dns")
	*p = targets
	return err
}

func parseTargets(n *yaml.Node, key string) ([]netip.AddrPort, error) {
	switch n.Kind {
	case yaml.ScalarNode:
		ap, err := netip.ParseAddrPort(n.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		return []netip.AddrPort{ap}, nil
	case yaml.SequenceNode:
		targets := make([]netip.AddrPort, 0, len(n.Content))
		for i, item := range n.Content {
			ap, err := netip.ParseAddrPort(item.Value)
			if item.Kind != yaml.ScalarNode || err != nil {
				return nil, fmt.Errorf("%s[%d]: want an ip:port, got %q", key, i, item.Value)
			}
			targets = append(targets, ap)
		}
		return targets, nil
	}
	return nil, fmt.Errorf("%s: want an ip:port or a list of them", key)
}

func (p DNSTargets) String() string { return ProbeTargets(p).String() }

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
		Health: Health{Interval: 15 * time.Second, Probe: ProbeTargets{netip.MustParseAddrPort("1.1.1.1:443")},
			DNS: DNSTargets{netip.MustParseAddrPort("1.1.1.1:53")}, DownAfter: 3, UpAfter: 2},
		History: History{MinuteRetention: 7 * 24 * time.Hour, HourRetention: 90 * 24 * time.Hour},
		Users:   Users{MaxDevices: DefaultMaxDevices},
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
	for i := range cfg.Upstreams {
		if cfg.Upstreams[i].Kind == "" {
			cfg.Upstreams[i].Kind = KindAWG
		}
	}
	if t := cfg.Health.Transfer; t != nil {
		t.MinBytes = cmp.Or(t.MinBytes, DefaultTransferMinBytes)
		t.Timeout = cmp.Or(t.Timeout, DefaultTransferTimeout)
		t.Interval = cmp.Or(t.Interval, DefaultTransferInterval)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var (
	// An upstream name becomes the TUN interface "lm-up-<name>", which must fit IFNAMSIZ (15).
	upstreamNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,8}$`)
	// A geo is an ISO 3166-1 alpha-2 country code, which the panel names.
	geoRe      = regexp.MustCompile(`^[A-Z]{2}$`)
	providerRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
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
	if err := validateEndpoint(c.Endpoint); err != nil {
		return fmt.Errorf("endpoint: %w", err)
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
	proxies := false
	for i, u := range c.Upstreams {
		if !upstreamNameRe.MatchString(u.Name) {
			return fmt.Errorf("upstreams[%d]: name %q must match %s (it becomes interface lm-up-<name>)", i, u.Name, upstreamNameRe)
		}
		if seen[u.Name] {
			return fmt.Errorf("upstreams[%d]: duplicate name %q", i, u.Name)
		}
		seen[u.Name] = true
		switch u.Kind {
		case "", KindAWG:
			if u.MaxFlows != 0 {
				return fmt.Errorf("upstream %q: max_flows applies to proxy upstreams only (kind %s or %s)", u.Name, KindOutline, KindVLESS)
			}
		case KindOutline, KindVLESS:
			proxies = true
			if u.MaxFlows < 0 {
				return fmt.Errorf("upstream %q: max_flows must not be negative", u.Name)
			}
		default:
			return fmt.Errorf("upstream %q: kind must be %s, %s or %s, got %q", u.Name, KindAWG, KindOutline, KindVLESS, u.Kind)
		}
		if u.Conf == "" {
			return fmt.Errorf("upstream %q: conf path is required", u.Name)
		}
		// Routes select upstreams by these two labels, so both are required.
		if !geoRe.MatchString(u.Geo) {
			return fmt.Errorf("upstream %q: geo %q must be an ISO 3166-1 alpha-2 country code in capitals, such as NL", u.Name, u.Geo)
		}
		if !providerRe.MatchString(u.Provider) {
			return fmt.Errorf("upstream %q: provider %q must match %s", u.Name, u.Provider, providerRe)
		}
	}
	if c.History.MinuteRetention < time.Hour {
		return fmt.Errorf("history.minute_retention: must be at least 1h")
	}
	if c.History.HourRetention < c.History.MinuteRetention {
		return fmt.Errorf("history.hour_retention: must not be shorter than minute_retention")
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
	if proxies {
		if len(h.DNS) == 0 {
			return fmt.Errorf("health.dns: at least one resolver is required for proxy upstreams (kind %s or %s)", KindOutline, KindVLESS)
		}
		// Clients must never get the address every proxy TUN carries.
		if c.Subnet.Contains(ProxyAddress.Addr()) {
			return fmt.Errorf("subnet: %s holds %s, the address of proxy upstreams; choose another subnet", c.Subnet, ProxyAddress.Addr())
		}
	}
	for i, t := range h.DNS {
		if !t.IsValid() {
			return fmt.Errorf("health.dns[%d]: invalid address", i)
		}
	}
	if h.DownAfter < 1 || h.UpAfter < 1 {
		return fmt.Errorf("health.down_after and health.up_after must be at least 1")
	}
	if t := h.Transfer; t != nil {
		if err := t.validate(); err != nil {
			return err
		}
	}
	if c.Users.MaxDevices < 0 {
		return fmt.Errorf("users.max_devices: must not be negative")
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

func (t *Transfer) validate() error {
	if len(t.URL) == 0 {
		return fmt.Errorf("health.transfer.url: at least one URL is required")
	}
	for i, raw := range t.URL {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return fmt.Errorf("health.transfer.url[%d]: want an http:// or https:// URL with a host, got %q", i, raw)
		}
	}
	if t.MinBytes <= 0 {
		return fmt.Errorf("health.transfer.min_bytes: must be positive")
	}
	if t.Timeout <= 0 || t.Interval <= 0 {
		return fmt.Errorf("health.transfer.timeout and health.transfer.interval: must be positive")
	}
	return nil
}

// validateEndpoint checks the address client configs connect to: an IP and
// a port, never a hostname. A client resolves a name before its tunnel is up,
// through whatever DNS it has, which may be blocked or poisoned; and a name
// may well point at a proxy that carries HTTP only, such as a CDN in front of
// the web panel. Either way the config would silently connect elsewhere.
func validateEndpoint(endpoint string) error {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return err
	}
	if host == "" {
		return errors.New("host is required")
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return fmt.Errorf("%q must be an IP address, not a hostname: clients would resolve it outside the tunnel", host)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return errors.New("port must be a number from 1 to 65535")
	}
	return nil
}
