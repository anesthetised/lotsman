package proxy

import (
	"bytes"
	"encoding/base64"
	"net/netip"
	"strings"
	"testing"
)

func TestParseShadowsocksKey(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	raw := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct {
		name, key, server, cipher string
		prefix                    []byte
	}{
		{name: "outline", key: "ss://" + raw("chacha20-ietf-poly1305:pa ss/+?") + "@vpn.example.org:443/?outline=1#My%20server",
			server: "vpn.example.org:443", cipher: "chacha20-ietf-poly1305"},
		{name: "padded standard base64", key: "ss://" + b64("aes-256-gcm:p@ss:w") + "@192.0.2.1:8388",
			server: "192.0.2.1:8388", cipher: "aes-256-gcm"},
		{name: "percent-encoded padding", key: "ss://" + strings.ReplaceAll(b64("aes-128-gcm:x"), "=", "%3D") + "@192.0.2.1:8388",
			server: "192.0.2.1:8388", cipher: "aes-128-gcm"},
		{name: "plain userinfo", key: "ss://AES-192-GCM:p%40ss%3Aw%2B@[2001:db8::1]:8388/",
			server: "[2001:db8::1]:8388", cipher: "aes-192-gcm"},
		{name: "legacy", key: "SS://" + b64("chacha20-ietf-poly1305:p@ss@192.0.2.1:8388") + "#name",
			server: "192.0.2.1:8388", cipher: "chacha20-ietf-poly1305"},
		// The TLS ClientHello disguise from Outline's documentation: U+00A8
		// in UTF-8 stands for the byte 0xA8.
		{name: "prefix", key: "ss://" + raw("chacha20-ietf-poly1305:x") + "@192.0.2.1:443/?outline=1&prefix=%16%03%01%00%C2%A8%01%01",
			server: "192.0.2.1:443", cipher: "chacha20-ietf-poly1305", prefix: []byte{0x16, 0x03, 0x01, 0x00, 0xa8, 0x01, 0x01}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, err := ParseKey("  " + tc.key + "\n")
			if err != nil {
				t.Fatal(err)
			}
			ss := k.(*shadowsocksKey)
			if ss.Server() != tc.server || ss.cipher != tc.cipher || !bytes.Equal(ss.prefix, tc.prefix) {
				t.Errorf("parsed %q %q % x, want %q %q % x", ss.Server(), ss.cipher, ss.prefix, tc.server, tc.cipher, tc.prefix)
			}
			if _, err := k.Dialers(netip.MustParseAddrPort("192.0.2.1:443")); err != nil {
				t.Errorf("dialers: %v", err)
			}
		})
	}
}

func TestParseKeyErrorsHideTheSecret(t *testing.T) {
	const secret = "hunter2secret"
	raw := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct{ name, key, want string }{
		{"no scheme", secret, "want a URL"},
		{"other scheme", "vless://" + secret + "@host:443", "unsupported scheme"},
		{"secret as scheme", secret + "://x", "unsupported scheme"},
		{"stream cipher", "ss://" + raw("aes-256-cfb:"+secret) + "@192.0.2.1:8388", "unsupported cipher"},
		{"password as cipher", "ss://" + secret + ":" + secret + "@192.0.2.1:8388", "unsupported cipher"},
		{"empty password", "ss://" + raw("aes-256-gcm:") + "@192.0.2.1:8388", "password is empty"},
		{"no colon", "ss://" + raw(secret) + "@192.0.2.1:8388", "method:password"},
		{"not base64", "ss://" + secret + "!@192.0.2.1:8388", "neither base64"},
		{"no port", "ss://" + raw("aes-256-gcm:"+secret) + "@192.0.2.1", "host:port"},
		{"port zero", "ss://" + raw("aes-256-gcm:"+secret) + "@192.0.2.1:0", "port"},
		{"legacy garbage", "ss://" + secret, "want ss://userinfo"},
		{"plugin", "ss://" + raw("aes-256-gcm:"+secret) + "@192.0.2.1:8388/?plugin=obfs-local", "plugins"},
		{"prefix too long", "ss://" + raw("aes-128-gcm:"+secret) + "@192.0.2.1:8388/?prefix=123456789", "at most 8"},
		{"prefix not bytes", "ss://" + raw("aes-256-gcm:"+secret) + "@192.0.2.1:8388/?prefix=%E2%82%AC", "one per byte"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseKey(tc.key)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error quotes the secret: %v", err)
			}
		})
	}
}
