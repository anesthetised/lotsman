package web

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte("public_url: https://vpn.example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8080" || cfg.Socket != "/run/lotsman/control.sock" || cfg.StateDir != "/var/lib/lotsman-web" {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.Origin() != "https://vpn.example.com" || len(cfg.trusted) != 0 {
		t.Errorf("origin %q, trusted %v", cfg.Origin(), cfg.trusted)
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
listen: "[::1]:9000"
public_url: https://Panel.Example.com:8443/
trusted_proxies: [127.0.0.1, "::1", 10.0.0.0/8]
socket: /tmp/control.sock
state_dir: /tmp/web
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Origin() != "https://panel.example.com:8443" {
		t.Errorf("origin = %q", cfg.Origin())
	}
	want := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("10.0.0.0/8")}
	if !slices.Equal(cfg.trusted, want) {
		t.Errorf("trusted = %v, want %v", cfg.trusted, want)
	}
	if cfg.Socket != "/tmp/control.sock" || cfg.StateDir != "/tmp/web" || cfg.Listen != "[::1]:9000" {
		t.Errorf("config = %+v", cfg)
	}
}

// The origin is what browsers send: lowercase host, no default port.
func TestOrigin(t *testing.T) {
	for raw, want := range map[string]string{
		"https://vpn.example.com":            "https://vpn.example.com",
		"https://vpn.example.com:443":        "https://vpn.example.com",
		"https://VPN.example.com:8443":       "https://vpn.example.com:8443",
		"http://localhost:8080":              "http://localhost:8080",
		"http://127.0.0.1:80/":               "http://127.0.0.1",
		"http://[::1]:8080":                  "http://[::1]:8080",
		"https://[2001:db8::1]:443":          "https://[2001:db8::1]",
		"https://xn--80ak6aa92e.example.com": "https://xn--80ak6aa92e.example.com",
	} {
		if got, err := origin(raw); err != nil || got != want {
			t.Errorf("origin(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestParseConfigRejects(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"listen: 127.0.0.1:8080\n", "public_url: required"},
		{"public_url: http://vpn.example.com\n", "must be https"},
		{"public_url: ftp://vpn.example.com\n", "must be https"},
		{"public_url: vpn.example.com\n", "public_url"},
		{"public_url: https://vpn.example.com/panel\n", "served at the root"},
		{"public_url: http://127.0.0.1:8080\n", "passkeys need a host name"},
		{"public_url: https://[2001:db8::1]\n", "passkeys need a host name"},
		{"public_url: https://vpn.example.com/?a=b\n", "served at the root"},
		{"public_url: https://user@vpn.example.com\n", "served at the root"},
		{"public_url: https://vpn.example.com\nlisten: 0.0.0.0:8080\n", "past the reverse proxy"},
		{"public_url: https://vpn.example.com\nlisten: '[::]:8080'\n", "past the reverse proxy"},
		{"public_url: https://vpn.example.com\nlisten: localhost:8080\n", "listen"},
		{"public_url: https://vpn.example.com\ntrusted_proxies: [proxy.local]\n", "neither an address nor a prefix"},
		{"public_url: https://vpn.example.com\ntrusted_proxies: [10.0.0.1/8]\n", "host bits"},
		{"public_url: https://vpn.example.com\nsocket: ''\n", "socket: required"},
		{"public_url: https://vpn.example.com\nstate_dir: ''\n", "state_dir: required"},
		{"public_url: https://vpn.example.com\nweb: {}\n", "field web not found"},
	} {
		_, err := ParseConfig([]byte(tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.yaml, err, tc.want)
		}
	}
}

func TestExampleConfig(t *testing.T) {
	cfg, err := LoadConfig("../../deploy/web.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Origin() != "https://vpn.example.com" || len(cfg.trusted) != 2 || cfg.Socket != DefaultConfig().Socket {
		t.Errorf("example = %+v", cfg)
	}
}
