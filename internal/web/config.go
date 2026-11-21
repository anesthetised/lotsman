package web

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/anesthetised/lotsman/internal/control"
)

// Config is web.yaml. It is separate from lotsman.yaml, which only root may
// read: the panel runs as lotsman-web and must not see the daemon's config.
type Config struct {
	// Listen is the numeric TCP address the panel serves on, normally a
	// loopback one behind the reverse proxy.
	Listen string `yaml:"listen"`
	// PublicURL is the origin browsers use, https://host[:port]. Origin
	// checks and the WebAuthn RP ID (its host) come from it, never from
	// Host. Passkeys are bound to that host: changing it means enrolling
	// again.
	PublicURL string `yaml:"public_url"`
	// TrustedProxies may set X-Forwarded-For; from anyone else it is ignored.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// Socket is the daemon's control socket.
	Socket string `yaml:"socket"`
	// StateDir holds web.db.
	StateDir string `yaml:"state_dir"`
	// OutlineUsage enables provider quota polling for named upstreams.
	OutlineUsage *OutlineUsageConfig `yaml:"outline_usage"`

	origin  string
	rpID    string
	trusted []netip.Prefix
}

func DefaultConfig() Config {
	return Config{Listen: "127.0.0.1:8080", Socket: control.SocketPath, StateDir: "/var/lib/lotsman-web"}
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(data)
}

func ParseConfig(data []byte) (*Config, error) {
	cfg := DefaultConfig()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse web config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Origin is PublicURL as browsers send it in the Origin header.
func (c *Config) Origin() string { return c.origin }

// EnrollURL is the page that redeems an enrollment token. The token is in
// the fragment, which browsers never send: it stays out of proxy logs.
func (c *Config) EnrollURL(token string) string { return c.origin + "/enroll#" + token }

func (c *Config) validate() error {
	ap, err := netip.ParseAddrPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if ap.Addr().IsUnspecified() {
		return fmt.Errorf("listen: %s would serve the panel past the reverse proxy; bind loopback", ap.Addr())
	}
	if c.origin, err = origin(c.PublicURL); err != nil {
		return fmt.Errorf("public_url: %w", err)
	}
	// Passkeys are bound to the host name, the WebAuthn RP ID; browsers
	// refuse an IP address as one.
	u, _ := url.Parse(c.origin)
	if c.rpID = u.Hostname(); net.ParseIP(c.rpID) != nil {
		return fmt.Errorf("public_url: %s is an IP address; passkeys need a host name, such as localhost", c.rpID)
	}
	c.trusted = nil
	for _, s := range c.TrustedProxies {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return fmt.Errorf("trusted_proxies: %q is neither an address nor a prefix", s)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p != p.Masked() {
			return fmt.Errorf("trusted_proxies: %s has host bits set; use %s", p, p.Masked())
		}
		c.trusted = append(c.trusted, p)
	}
	if c.Socket == "" {
		return fmt.Errorf("socket: required")
	}
	if c.StateDir == "" {
		return fmt.Errorf("state_dir: required")
	}
	if c.OutlineUsage != nil {
		return c.OutlineUsage.validate()
	}
	return nil
}

// origin reduces a public URL to its origin, scheme://host[:port], with the
// default port left out as browsers do. It must be https, but for loopback,
// where browsers treat plain http as secure.
func origin(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("required, such as https://vpn.example.com")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q: want scheme://host[:port] and nothing else; the panel is served at the root", raw)
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	switch u.Scheme {
	case "https":
	case "http":
		if a, err := netip.ParseAddr(host); host != "localhost" && (err != nil || !a.IsLoopback()) {
			return "", fmt.Errorf("%q: must be https (plain http only on loopback)", raw)
		}
	default:
		return "", fmt.Errorf("%q: must be https", raw)
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return u.Scheme + "://" + host, nil
}
