// Package awgconf reads and writes AmneziaWG configuration files (the WireGuard
// INI format plus AmneziaWG obfuscation keys) and renders amneziawg-go UAPI.
package awgconf

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

type Interface struct {
	PrivateKey Key
	Addresses  []netip.Prefix
	DNS        []netip.Addr
	ListenPort uint16
	MTU        int
	Params
}

type Peer struct {
	PublicKey           Key
	PresharedKey        Key
	Endpoint            string
	AllowedIPs          []netip.Prefix
	PersistentKeepalive uint32
}

type Config struct {
	Interface Interface
	Peers     []Peer
}

// wg-quick keys that describe how to bring the interface up on the client
// machine. They carry nothing Lotsman needs, so they are skipped rather than rejected.
var ignoredKeys = map[string]bool{
	"table": true, "preup": true, "postup": true, "predown": true, "postdown": true,
	"saveconfig": true, "fwmark": true,
}

func Parse(r io.Reader) (*Config, error) {
	cfg := &Config{}
	var section string
	var peer *Peer
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(line[1 : len(line)-1])
			switch section {
			case "interface":
			case "peer":
				cfg.Peers = append(cfg.Peers, Peer{})
				peer = &cfg.Peers[len(cfg.Peers)-1]
			default:
				return nil, fmt.Errorf("line %d: unknown section %q", n, line)
			}
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("line %d: expected key = value", n)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		var err error
		switch section {
		case "interface":
			err = cfg.Interface.set(key, value)
		case "peer":
			err = peer.set(key, value)
		default:
			err = fmt.Errorf("key outside of a section")
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if cfg.Interface.PrivateKey.IsZero() {
		return nil, fmt.Errorf("[Interface] has no PrivateKey")
	}
	return cfg, nil
}

func (iface *Interface) set(key, value string) (err error) {
	switch key {
	case "privatekey":
		iface.PrivateKey, err = ParseKey(value)
	case "address":
		iface.Addresses, err = parseList(value, parsePrefix)
	case "dns":
		iface.DNS, err = parseList(value, netip.ParseAddr)
	case "listenport":
		iface.ListenPort, err = parseUint16(value)
	case "mtu":
		iface.MTU, err = strconv.Atoi(value)
	case "jc":
		iface.Jc, err = parseUint32(value)
	case "jmin":
		iface.Jmin, err = parseUint32(value)
	case "jmax":
		iface.Jmax, err = parseUint32(value)
	case "s1":
		iface.S1, err = parseUint16(value)
	case "s2":
		iface.S2, err = parseUint16(value)
	case "s3":
		iface.S3, err = parseUint16(value)
	case "s4":
		iface.S4, err = parseUint16(value)
	case "h1":
		iface.H1, err = ParseRange(value)
	case "h2":
		iface.H2, err = ParseRange(value)
	case "h3":
		iface.H3, err = ParseRange(value)
	case "h4":
		iface.H4, err = ParseRange(value)
	case "i1", "i2", "i3", "i4", "i5":
		iface.I[key[1]-'1'] = value
	default:
		if !ignoredKeys[key] {
			return fmt.Errorf("unsupported [Interface] key %q", key)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func (p *Peer) set(key, value string) (err error) {
	switch key {
	case "publickey":
		p.PublicKey, err = ParseKey(value)
	case "presharedkey":
		p.PresharedKey, err = ParseKey(value)
	case "endpoint":
		p.Endpoint = value
	case "allowedips":
		p.AllowedIPs, err = parseList(value, parsePrefix)
	case "persistentkeepalive":
		p.PersistentKeepalive, err = parseUint32(value)
	default:
		return fmt.Errorf("unsupported [Peer] key %q", key)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

// String renders the config in the format the Amnezia client imports.
func (c *Config) String() string {
	var b strings.Builder
	line := func(k string, v any) { fmt.Fprintf(&b, "%s = %v\n", k, v) }
	iface := c.Interface
	b.WriteString("[Interface]\n")
	line("PrivateKey", iface.PrivateKey)
	if len(iface.Addresses) > 0 {
		line("Address", joinList(iface.Addresses))
	}
	if len(iface.DNS) > 0 {
		line("DNS", joinList(iface.DNS))
	}
	if iface.ListenPort != 0 {
		line("ListenPort", iface.ListenPort)
	}
	if iface.MTU != 0 {
		line("MTU", iface.MTU)
	}
	if iface.Jc != 0 || iface.Jmin != 0 || iface.Jmax != 0 {
		line("Jc", iface.Jc)
		line("Jmin", iface.Jmin)
		line("Jmax", iface.Jmax)
	}
	line("S1", iface.S1)
	line("S2", iface.S2)
	line("S3", iface.S3)
	line("S4", iface.S4)
	for i, r := range [4]Range{iface.H1, iface.H2, iface.H3, iface.H4} {
		if !r.IsZero() {
			line(fmt.Sprintf("H%d", i+1), r)
		}
	}
	for i, v := range iface.I {
		if v != "" {
			line(fmt.Sprintf("I%d", i+1), v)
		}
	}
	for _, p := range c.Peers {
		b.WriteString("\n[Peer]\n")
		line("PublicKey", p.PublicKey)
		if !p.PresharedKey.IsZero() {
			line("PresharedKey", p.PresharedKey)
		}
		if p.Endpoint != "" {
			line("Endpoint", p.Endpoint)
		}
		if len(p.AllowedIPs) > 0 {
			line("AllowedIPs", joinList(p.AllowedIPs))
		}
		if p.PersistentKeepalive != 0 {
			line("PersistentKeepalive", p.PersistentKeepalive)
		}
	}
	return b.String()
}

// UAPI renders the whole config as an amneziawg-go IpcSet payload.
func (c *Config) UAPI() string {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", c.Interface.PrivateKey.Hex())
	if c.Interface.ListenPort != 0 {
		fmt.Fprintf(&b, "listen_port=%d\n", c.Interface.ListenPort)
	}
	b.WriteString(c.Interface.Params.UAPI())
	for _, p := range c.Peers {
		b.WriteString(p.UAPI())
	}
	return b.String()
}

// UAPI renders one peer as IpcSet lines; it can be sent alone to add or update the peer.
func (p Peer) UAPI() string {
	var b strings.Builder
	fmt.Fprintf(&b, "public_key=%s\n", p.PublicKey.Hex())
	if !p.PresharedKey.IsZero() {
		fmt.Fprintf(&b, "preshared_key=%s\n", p.PresharedKey.Hex())
	}
	if p.Endpoint != "" {
		fmt.Fprintf(&b, "endpoint=%s\n", p.Endpoint)
	}
	b.WriteString("replace_allowed_ips=true\n")
	for _, ip := range p.AllowedIPs {
		fmt.Fprintf(&b, "allowed_ip=%s\n", ip)
	}
	if p.PersistentKeepalive != 0 {
		fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", p.PersistentKeepalive)
	}
	return b.String()
}

func parseList[T any](s string, parse func(string) (T, error)) ([]T, error) {
	var out []T
	for _, item := range strings.Split(s, ",") {
		v, err := parse(strings.TrimSpace(item))
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// parsePrefix accepts both "10.0.0.2/32" and a bare "10.0.0.2".
func parsePrefix(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func joinList[T fmt.Stringer](items []T) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = it.String()
	}
	return strings.Join(parts, ", ")
}

func parseUint16(s string) (uint16, error) {
	v, err := strconv.ParseUint(s, 10, 16)
	return uint16(v), err
}

func parseUint32(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	return uint32(v), err
}
