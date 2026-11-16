package proxy

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"golang.getoutline.org/sdk/transport"
	"golang.getoutline.org/sdk/transport/shadowsocks"
)

// shadowsocksKey is a parsed ss:// key, as Outline servers hand out.
type shadowsocksKey struct {
	server string
	cipher string
	key    *shadowsocks.EncryptionKey
	prefix []byte // the first bytes of every TCP salt; Outline's "prefix" disguise
}

// parseShadowsocks reads what follows "ss://": SIP002
// (userinfo@host:port, where userinfo is base64url("method:password") or
// percent-encoded "method:password"), or the legacy
// base64("method:password@host:port"). The fragment, a display name, is
// ignored; of the query, only Outline's "prefix" is used. No error quotes
// the key.
func parseShadowsocks(rest string) (*shadowsocksKey, error) {
	rest, _, _ = strings.Cut(rest, "#")
	rest, query, _ := strings.Cut(rest, "?")
	rest = strings.TrimSuffix(rest, "/")

	var method, password, server string
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		var err error
		if method, password, err = sip002UserInfo(rest[:i]); err != nil {
			return nil, err
		}
		server = rest[i+1:]
	} else {
		decoded, ok := decodeBase64(rest)
		i := strings.LastIndex(decoded, "@")
		if !ok || i < 0 {
			return nil, errors.New("ss key: want ss://userinfo@host:port or legacy ss://base64")
		}
		var found bool
		method, password, found = strings.Cut(decoded[:i], ":")
		if !found {
			return nil, errors.New("ss key: credentials must be method:password")
		}
		server = decoded[i+1:]
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return nil, errors.New("ss key: malformed query")
	}
	if values.Has("plugin") {
		return nil, errors.New("ss key: plugins are not supported")
	}
	return newShadowsocksKey(server, method, password, values.Get("prefix"))
}

// newShadowsocksKey checks a key's parts, however they were written. The
// prefix is Outline's, decoded from any URL escaping. No error quotes the
// password.
func newShadowsocksKey(server, method, password, prefixText string) (*shadowsocksKey, error) {
	if err := splitServer(server); err != nil {
		return nil, fmt.Errorf("ss key: %w", err)
	}
	if password == "" {
		return nil, errors.New("ss key: password is empty")
	}
	method = strings.ToLower(method)
	key, err := shadowsocks.NewEncryptionKey(method, password)
	if err != nil {
		// Not quoted: a malformed key may put the password here.
		return nil, errors.New("ss key: unsupported cipher; want chacha20-ietf-poly1305, aes-256-gcm, aes-192-gcm or aes-128-gcm (AEAD only)")
	}
	prefix, err := parsePrefix(prefixText)
	if err != nil {
		return nil, err
	}
	// The SDK fills the rest of the salt with random bytes. Keep at least
	// half of it random: a salt that repeats exposes the key.
	if len(prefix) > key.SaltSize()/2 {
		return nil, fmt.Errorf("ss key: prefix is %d bytes, at most %d with %s", len(prefix), key.SaltSize()/2, method)
	}
	return &shadowsocksKey{server: server, cipher: method, key: key, prefix: prefix}, nil
}

// sip002UserInfo decodes "method:password", percent-encoded or in base64.
func sip002UserInfo(userinfo string) (method, password string, err error) {
	if m, p, ok := strings.Cut(userinfo, ":"); ok {
		method, err1 := url.PathUnescape(m)
		password, err2 := url.PathUnescape(p)
		if err1 != nil || err2 != nil {
			return "", "", errors.New("ss key: malformed percent-encoding in credentials")
		}
		return method, password, nil
	}
	unescaped, err := url.PathUnescape(userinfo) // padding may come as %3D
	if err != nil {
		return "", "", errors.New("ss key: malformed percent-encoding in credentials")
	}
	decoded, ok := decodeBase64(unescaped)
	if !ok {
		return "", "", errors.New("ss key: credentials are neither base64 nor method:password")
	}
	method, password, ok = strings.Cut(decoded, ":")
	if !ok {
		return "", "", errors.New("ss key: credentials must be method:password")
	}
	return method, password, nil
}

// decodeBase64 accepts both alphabets, with or without padding: clients
// and servers disagree on which one SIP002 means.
func decodeBase64(s string) (string, bool) {
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), true
		}
	}
	return "", false
}

// parsePrefix turns Outline's prefix parameter into bytes: after
// percent-decoding, each character is one byte value, so U+00A8 is 0xA8.
func parsePrefix(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			return nil, errors.New("ss key: prefix characters must be in U+0000–U+00FF, one per byte")
		}
		out = append(out, byte(r))
	}
	return out, nil
}

func (k *shadowsocksKey) Server() string { return k.server }

func (k *shadowsocksKey) String() string {
	s := "shadowsocks " + k.cipher
	if len(k.prefix) > 0 {
		s += fmt.Sprintf(", %d-byte prefix", len(k.prefix))
	}
	return s
}

// Dialers dial addr directly over the host's network. Like Outline's own
// client, only TCP salts carry the prefix.
func (k *shadowsocksKey) Dialers(addr netip.AddrPort) (Dialers, error) {
	streams, err := shadowsocks.NewStreamDialer(&transport.TCPEndpoint{Address: addr.String()}, k.key)
	if err != nil {
		return Dialers{}, err
	}
	if len(k.prefix) > 0 {
		streams.SaltGenerator = shadowsocks.NewPrefixSaltGenerator(k.prefix)
	}
	packets, err := shadowsocks.NewPacketListener(transport.UDPEndpoint{Address: addr.String()}, k.key)
	if err != nil {
		return Dialers{}, err
	}
	return Dialers{Streams: streams, Packets: packets}, nil
}
