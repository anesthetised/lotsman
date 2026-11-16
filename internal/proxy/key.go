package proxy

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Key is a proxy access key: the server to reach and how to dial through
// it. Keys are secrets: only Dialers uses the secret part.
type Key interface {
	// Server is the server's host:port as the key gives it.
	Server() string
	// Dialers reach the server at addr, Server resolved.
	Dialers(addr netip.AddrPort) (Dialers, error)
	// String describes the key without its secret, for logs and config check.
	String() string
}

// ParseKey reads an access key; ss:// is the only kind so far. No error
// quotes any part of the key.
func ParseKey(s string) (Key, error) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(s), "://")
	if !ok {
		return nil, errors.New("access key: want a URL such as ss://…")
	}
	if strings.EqualFold(scheme, "ss") {
		return parseShadowsocks(rest)
	}
	return nil, errors.New("access key: unsupported scheme, want ss://")
}

// splitServer checks a server's host:port without resolving anything.
func splitServer(hostport string) error {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || host == "" {
		return errors.New("server must be host:port")
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return errors.New("server port must be a number from 1 to 65535")
	}
	return nil
}
