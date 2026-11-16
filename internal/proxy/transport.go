package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"golang.getoutline.org/sdk/transport"
)

// The part of Outline's client config language read here, as the Outline
// client parses it (outline-apps client/go/outline/configregistry:
// registry.go, NewDefaultTransportProvider; config_tcpudp.go;
// config_shadowsocks.go; config_dial_endpoint.go; config_websocket.go). A
// map's "$type" picks its parser:
//
//	transport: ss://… | {$type: tcpudp, tcp: D, udp: D} | S
//	D:         ss://… | {$type: shadowsocks, …S}
//	S:         {endpoint: E, cipher, secret, prefix} | {server, server_port, method, password, prefix}
//	E:         host:port | {$type: dial, address} | {$type: websocket, url, endpoint?: host:port | {$type: dial, address}}
//
// What the Outline client would send around the server is refused: direct
// TCP, chained dialers, and every other $type. A missing udp, or a direct
// one, leaves the key without UDP rather than sending UDP around the server.

// parseOutlineConfig reads an Outline config object: its transport, or the
// object itself when it has none, which is how Outline reads the legacy
// Shadowsocks object (outline-apps client/go/outline/parse.go,
// doParseTunnelConfig). Other keys, such as a usage reporter, are ignored.
func parseOutlineConfig(v map[string]any) (Key, error) {
	node, ok := v["transport"]
	if !ok {
		node = v
	}
	switch n := node.(type) {
	case string:
		return parseSSURL(n)
	case map[string]any:
		typ, err := typeOf(n)
		if err != nil {
			return nil, err
		}
		switch typ {
		case "": // what Outline does without a $type
			return parseShadowsocksConfig(n)
		case "tcpudp":
			return parseTCPUDP(n)
		default:
			return nil, unsupported("transport", typ)
		}
	default:
		return nil, errors.New("transport must be an ss:// key or an object")
	}
}

func parseTCPUDP(n map[string]any) (Key, error) {
	if n["tcp"] == nil || isDirect(n["tcp"]) {
		return nil, errors.New("tcp: direct access, without a proxy, is not supported")
	}
	tcp, err := parseDialer(n["tcp"])
	if err != nil {
		return nil, fmt.Errorf("tcp: %w", err)
	}
	k := &transportKey{tcp: tcp}
	if n["udp"] != nil && !isDirect(n["udp"]) {
		if k.udp, err = parseDialer(n["udp"]); err != nil {
			return nil, fmt.Errorf("udp: %w", err)
		}
		if !sameHost(tcp.server, k.udp.server) {
			return nil, errors.New("tcp and udp must reach the same server host: an upstream has one server address")
		}
	}
	return k, nil
}

// parseDialer reads tcpudp's tcp or udp.
func parseDialer(node any) (*shadowsocksKey, error) {
	switch n := node.(type) {
	case string:
		return parseSSURL(n)
	case map[string]any:
		typ, err := typeOf(n)
		if err != nil {
			return nil, err
		}
		if typ != "shadowsocks" {
			return nil, unsupported("dialer", typ)
		}
		return parseShadowsocksConfig(n)
	default:
		return nil, errors.New("want an ss:// key or a shadowsocks object")
	}
}

// parseShadowsocksConfig reads a Shadowsocks object, the current form with
// an endpoint or the legacy one with a server.
func parseShadowsocksConfig(n map[string]any) (*shadowsocksKey, error) {
	if _, ok := n["endpoint"]; !ok {
		if _, ok := n["server"]; ok {
			return legacyShadowsocks(n)
		}
		return nil, errors.New("shadowsocks: endpoint (or server) is missing")
	}
	server, ws, err := parseEndpoint(n["endpoint"], true)
	if err != nil {
		return nil, fmt.Errorf("shadowsocks: %w", err)
	}
	var fields [3]string
	for i, name := range []string{"cipher", "secret", "prefix"} {
		if fields[i], err = stringField(n, name, name != "prefix"); err != nil {
			return nil, err
		}
	}
	k, err := newShadowsocksKey(server, fields[0], fields[1], fields[2])
	if err != nil {
		return nil, err
	}
	k.ws = ws
	return k, nil
}

// parseEndpoint reads a Shadowsocks endpoint and returns the host:port to
// connect to and, for a websocket endpoint, the WebSocket.
func parseEndpoint(node any, websocket bool) (string, *wsEndpoint, error) {
	switch n := node.(type) {
	case string:
		if err := splitServer(n); err != nil {
			return "", nil, fmt.Errorf("endpoint: %w", err)
		}
		return n, nil, nil
	case map[string]any:
		typ, err := typeOf(n)
		if err != nil {
			return "", nil, err
		}
		switch {
		case typ == "" || typ == "dial":
			if d := n["dialer"]; d != nil && !isDirect(d) {
				return "", nil, errors.New("endpoint: chained dialers are not supported")
			}
			address, err := stringField(n, "address", true)
			if err != nil {
				return "", nil, fmt.Errorf("endpoint: %w", err)
			}
			return parseEndpoint(address, false)
		case typ == "websocket" && websocket:
			rawURL, err := stringField(n, "url", true)
			if err != nil {
				return "", nil, fmt.Errorf("websocket endpoint: %w", err)
			}
			ws, server, err := parseWSURL(rawURL)
			if err != nil {
				return "", nil, err
			}
			if inner := n["endpoint"]; inner != nil {
				if server, _, err = parseEndpoint(inner, false); err != nil {
					return "", nil, fmt.Errorf("websocket %w", err)
				}
			}
			return server, ws, nil
		default:
			return "", nil, unsupported("endpoint", typ)
		}
	default:
		return "", nil, errors.New("endpoint must be host:port or an object")
	}
}

// parseSSURL reads an ss:// key given inside a config.
func parseSSURL(s string) (*shadowsocksKey, error) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(s), "://")
	if !ok || !strings.EqualFold(scheme, "ss") {
		return nil, errors.New("want an ss:// key")
	}
	return parseShadowsocks(rest)
}

// typeOf is a config object's $type, "" without one.
func typeOf(n map[string]any) (string, error) {
	t, ok := n["$type"]
	if !ok {
		return "", nil
	}
	s, ok := t.(string)
	if !ok {
		return "", errors.New("$type must be a string")
	}
	return s, nil
}

func isDirect(node any) bool {
	n, ok := node.(map[string]any)
	return ok && n["$type"] == "direct"
}

// unsupported refuses a $type. The name is quoted only when it looks like
// one, since a mangled config could put anything there.
func unsupported(what, typ string) error {
	shown := "(not shown)"
	if len(typ) <= 32 && strings.Trim(typ, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") == "" {
		shown = strconv.Quote(typ)
	}
	if typ == "" {
		shown = "without $type"
	}
	return fmt.Errorf("%s %s is not supported", what, shown)
}

// stringField reads a string field of a config object. Its value is never
// quoted: it may be a secret.
func stringField(n map[string]any, name string, required bool) (string, error) {
	raw, ok := n[name]
	if !ok || raw == nil {
		if required {
			return "", fmt.Errorf("%s is missing", name)
		}
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return s, nil
}

func sameHost(a, b string) bool {
	ha, _, _ := net.SplitHostPort(a)
	hb, _, _ := net.SplitHostPort(b)
	return strings.EqualFold(ha, hb)
}

// transportKey is Outline's tcpudp transport: separate Shadowsocks
// transports for TCP and UDP, on one server host, which the daemon resolves
// once; each keeps its own port.
type transportKey struct {
	tcp, udp *shadowsocksKey // udp nil: the config has no UDP transport
}

func (k *transportKey) Server() string { return k.tcp.server }

func (k *transportKey) String() string {
	if k.udp == nil {
		return "tcp: " + k.tcp.String() + "; udp: none"
	}
	if tcp, udp := k.tcp.String(), k.udp.String(); tcp != udp {
		return "tcp: " + tcp + "; udp: " + udp
	}
	return k.tcp.String()
}

// Dialers reach TCP at addr, the server resolved, and UDP at the same
// address with the UDP transport's port. Without a UDP transport, every UDP
// flow fails, and with it the upstream's health.
func (k *transportKey) Dialers(addr netip.AddrPort) (Dialers, error) {
	streams, err := k.tcp.streams(addr)
	if err != nil {
		return Dialers{}, err
	}
	var packets transport.PacketListener = noUDP{}
	if k.udp != nil {
		_, port, _ := net.SplitHostPort(k.udp.server)
		n, _ := strconv.ParseUint(port, 10, 16) // checked when parsed
		if packets, err = k.udp.packets(netip.AddrPortFrom(addr.Addr(), uint16(n)), true); err != nil {
			return Dialers{}, err
		}
	}
	return Dialers{Streams: streams, Packets: packets}, nil
}

// noUDP is the packet listener of a key without a UDP transport.
type noUDP struct{}

func (noUDP) ListenPacket(context.Context) (net.PacketConn, error) {
	return nil, errors.New("the access key has no UDP transport")
}
