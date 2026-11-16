package proxy

import (
	"bytes"
	"context"
	"net/netip"
	"strings"
	"testing"
)

const (
	wsSecret = "hunter2wssecret"      // a Shadowsocks secret in these configs
	wsPath   = "/s3cr3t-ws-token/tcp" // the secret part of their URLs
)

// tcpudp is an Outline config of the shape providers serve for WebSocket
// keys (Outline's own docs and outline-ss-server's config_example.yml).
func tcpudp(tcpEndpoint, udpEndpoint string) string {
	side := func(endpoint string) string {
		return `{"$type": "shadowsocks", "endpoint": ` + endpoint + `, "cipher": "chacha20-ietf-poly1305", "secret": "` + wsSecret + `"}`
	}
	return `{"transport": {"$type": "tcpudp", "tcp": ` + side(tcpEndpoint) + `, "udp": ` + side(udpEndpoint) + `}}`
}

func TestParseTransportConfig(t *testing.T) {
	ss := ssKey("192.0.2.1:8388")
	for name, tc := range map[string]struct {
		text, server, desc string
		tcpWS, udpWS       string // the URLs dialed, "" for plain Shadowsocks
		udpServer          string // "" when UDP goes where TCP does; "none" without UDP
	}{
		"websocket yaml": {
			text: `
transport:
  $type: tcpudp
  tcp:
    $type: shadowsocks
    endpoint:
      $type: websocket
      url: wss://ws.example.org` + wsPath + `
    cipher: chacha20-ietf-poly1305
    secret: ` + wsSecret + `
  udp:
    $type: shadowsocks
    endpoint: {$type: websocket, url: "wss://ws.example.org/s3cr3t-ws-token/udp#frag"}
    cipher: chacha20-ietf-poly1305
    secret: ` + wsSecret + `
reporter: {$type: http, url: https://report.example.org}
`,
			server: "ws.example.org:443", desc: "shadowsocks chacha20-ietf-poly1305 over WebSocket",
			tcpWS: "wss://ws.example.org" + wsPath, udpWS: "wss://ws.example.org/s3cr3t-ws-token/udp"},
		"websocket json, ports, https alias": {
			text:   tcpudp(`{"$type": "websocket", "url": "https://ws.example.org:8443`+wsPath+`"}`, `{"$type": "websocket", "url": "ws://WS.example.org/udp"}`),
			server: "ws.example.org:8443", desc: "shadowsocks chacha20-ietf-poly1305 over WebSocket",
			tcpWS: "wss://ws.example.org:8443" + wsPath, udpWS: "ws://WS.example.org/udp", udpServer: "WS.example.org:80"},
		"websocket at another address": {
			text:   tcpudp(`{"$type": "websocket", "url": "wss://ws.example.org`+wsPath+`", "endpoint": "198.51.100.4:443"}`, `{"$type": "websocket", "url": "wss://ws.example.org/udp", "endpoint": {"$type": "dial", "address": "198.51.100.4:443"}}`),
			server: "198.51.100.4:443", desc: "shadowsocks chacha20-ietf-poly1305 over WebSocket",
			tcpWS: "wss://ws.example.org" + wsPath, udpWS: "wss://ws.example.org/udp"},
		"no udp": {
			text:   `{"transport": {"$type": "tcpudp", "tcp": "` + ss + `"}}`,
			server: "192.0.2.1:8388", desc: "tcp: shadowsocks chacha20-ietf-poly1305; udp: none", udpServer: "none"},
		"direct udp": {
			text:   `{"transport": {"$type": "tcpudp", "tcp": "` + ss + `", "udp": {"$type": "direct"}}}`,
			server: "192.0.2.1:8388", desc: "tcp: shadowsocks chacha20-ietf-poly1305; udp: none", udpServer: "none"},
		"plain tcpudp": {
			text:   tcpudp(`"192.0.2.1:8388"`, `{"$type": "dial", "address": "192.0.2.1:8389"}`),
			server: "192.0.2.1:8388", desc: "shadowsocks chacha20-ietf-poly1305", udpServer: "192.0.2.1:8389"},
		"shadowsocks transport": {
			text:   `{"transport": {"endpoint": {"address": "192.0.2.1:8388"}, "cipher": "aes-256-gcm", "secret": "` + wsSecret + `", "prefix": "\u0016\u0003"}}`,
			server: "192.0.2.1:8388", desc: "shadowsocks aes-256-gcm, 2-byte prefix"},
		"transport string": {
			text:   `{"transport": "` + ss + `"}`,
			server: "192.0.2.1:8388", desc: "shadowsocks chacha20-ietf-poly1305"},
		"top-level tcpudp": {
			text:   `{"$type": "tcpudp", "tcp": "` + ss + `", "udp": "` + ss + `"}`,
			server: "192.0.2.1:8388", desc: "shadowsocks chacha20-ietf-poly1305"},
		"top-level websocket": {
			text:   `{"endpoint": {"$type": "websocket", "url": "wss://ws.example.org` + wsPath + `"}, "cipher": "chacha20-ietf-poly1305", "secret": "` + wsSecret + `"}`,
			server: "ws.example.org:443", desc: "shadowsocks chacha20-ietf-poly1305 over WebSocket",
			tcpWS: "wss://ws.example.org" + wsPath, udpWS: "wss://ws.example.org" + wsPath},
	} {
		t.Run(name, func(t *testing.T) {
			k, err := ParseConfig(tc.text)
			if err != nil {
				t.Fatal(err)
			}
			if k.Server() != tc.server || k.String() != tc.desc {
				t.Errorf("server %q, %q; want %q, %q", k.Server(), k, tc.server, tc.desc)
			}
			if strings.Contains(k.String(), wsSecret) || strings.Contains(k.String(), "s3cr3t") {
				t.Errorf("String shows a secret: %s", k)
			}
			tcp, udp := sides(k)
			if url := wsURL(tcp); url != tc.tcpWS {
				t.Errorf("tcp websocket %q, want %q", url, tc.tcpWS)
			}
			if tc.udpServer == "none" {
				if udp != nil {
					t.Error("udp transport parsed from a config without one")
				}
			} else if url := wsURL(udp); url != tc.udpWS {
				t.Errorf("udp websocket %q, want %q", url, tc.udpWS)
			} else if want := cmpOr(tc.udpServer, tc.server); udp.server != want {
				t.Errorf("udp server %q, want %q", udp.server, want)
			}
			if _, err := k.Dialers(netip.MustParseAddrPort("192.0.2.1:443")); err != nil {
				t.Errorf("dialers: %v", err)
			}
		})
	}
}

// sides returns the Shadowsocks transports a key uses for TCP and UDP.
func sides(k Key) (tcp, udp *shadowsocksKey) {
	switch k := k.(type) {
	case *shadowsocksKey:
		return k, k
	case *transportKey:
		return k.tcp, k.udp
	}
	panic("unknown key type")
}

func wsURL(k *shadowsocksKey) string {
	if k == nil || k.ws == nil {
		return ""
	}
	return k.ws.url
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// The prefix applies to TCP, and to UDP only in a transport of its own, as
// in the Outline client.
func TestTransportPrefix(t *testing.T) {
	k, err := ParseConfig(`{"transport": {"$type": "tcpudp",
		"tcp": {"$type": "shadowsocks", "endpoint": "192.0.2.1:8388", "cipher": "aes-256-gcm", "secret": "x", "prefix": "\u0016"},
		"udp": {"$type": "shadowsocks", "endpoint": "192.0.2.1:8388", "cipher": "aes-256-gcm", "secret": "x", "prefix": "¨"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	tcp, udp := sides(k)
	if !bytes.Equal(tcp.prefix, []byte{0x16}) || !bytes.Equal(udp.prefix, []byte{0xa8}) {
		t.Errorf("prefixes % x, % x", tcp.prefix, udp.prefix)
	}
}

func TestNoUDPTransport(t *testing.T) {
	k, err := ParseConfig(`{"transport": {"$type": "tcpudp", "tcp": "` + ssKey("192.0.2.1:8388") + `"}}`)
	if err != nil {
		t.Fatal(err)
	}
	d, err := k.Dialers(netip.MustParseAddrPort("192.0.2.1:8388"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Packets.ListenPacket(context.Background()); err == nil || !strings.Contains(err.Error(), "no UDP transport") {
		t.Errorf("listen without a UDP transport: %v", err)
	}
}

func TestParseTransportConfigErrors(t *testing.T) {
	ws := func(url string) string { return `{"$type": "websocket", "url": "` + url + `"}` }
	ok := ws("wss://ws.example.org" + wsPath)
	side := func(fields string) string {
		return `{"transport": {"$type": "tcpudp", "tcp": {"$type": "shadowsocks", ` + fields + `}}}`
	}
	for name, tc := range map[string]struct{ text, want string }{
		"transport type":    {`{"transport": {"$type": "basic-access", "secret": "` + wsSecret + `"}}`, `transport "basic-access" is not supported`},
		"odd type":          {`{"transport": {"$type": "` + wsSecret + `!"}}`, "transport (not shown) is not supported"},
		"type not a string": {`{"transport": {"$type": 1}}`, "$type must be a string"},
		"transport list":    {`{"transport": ["` + wsSecret + `"]}`, "transport must be"},
		"transport url":     {`{"transport": "ws://` + wsSecret + `"}`, "want an ss:// key"},
		"no tcp":            {`{"transport": {"$type": "tcpudp", "udp": "` + ssKey("192.0.2.1:8388") + `"}}`, "tcp: direct access"},
		"direct tcp":        {`{"transport": {"$type": "tcpudp", "tcp": {"$type": "direct"}}}`, "tcp: direct access"},
		"tcp iptable":       {`{"transport": {"$type": "tcpudp", "tcp": {"$type": "iptable", "secret": "` + wsSecret + `"}}}`, `tcp: dialer "iptable" is not supported`},
		"tcp untyped":       {`{"transport": {"$type": "tcpudp", "tcp": {"endpoint": "h:1", "secret": "` + wsSecret + `"}}}`, "tcp: dialer without $type is not supported"},
		"udp first-supported": {`{"transport": {"$type": "tcpudp", "tcp": "` + ssKey("192.0.2.1:8388") + `", "udp": {"$type": "first-supported", "options": ["` + wsSecret + `"]}}}`,
			`udp: dialer "first-supported" is not supported`},
		"no endpoint":     {side(`"cipher": "aes-256-gcm", "secret": "` + wsSecret + `"`), "endpoint (or server) is missing"},
		"no cipher":       {side(`"endpoint": ` + ok + `, "secret": "` + wsSecret + `"`), "cipher is missing"},
		"no secret":       {side(`"endpoint": ` + ok + `, "cipher": "aes-256-gcm"`), "secret is missing"},
		"empty secret":    {side(`"endpoint": ` + ok + `, "cipher": "aes-256-gcm", "secret": ""`), "password is empty"},
		"bad cipher":      {side(`"endpoint": ` + ok + `, "cipher": "` + wsSecret + `", "secret": "` + wsSecret + `"`), "unsupported cipher"},
		"secret a number": {side(`"endpoint": ` + ok + `, "cipher": "aes-256-gcm", "secret": 12345`), "secret must be a string"},
		"long prefix":     {side(`"endpoint": ` + ok + `, "cipher": "aes-128-gcm", "secret": "` + wsSecret + `", "prefix": "123456789"`), "at most 8"},
		"endpoint port":   {side(`"endpoint": "` + wsSecret + `", "cipher": "aes-256-gcm", "secret": "x"`), "host:port"},
		"endpoint list":   {side(`"endpoint": ["` + wsSecret + `"], "cipher": "aes-256-gcm", "secret": "x"`), "endpoint must be"},
		"endpoint tls":    {side(`"endpoint": {"$type": "tls", "sni": "` + wsSecret + `"}, "cipher": "aes-256-gcm", "secret": "x"`), `endpoint "tls" is not supported`},
		"chained dialer": {side(`"endpoint": {"$type": "dial", "address": "h:1", "dialer": {"$type": "shadowsocks", "secret": "` + wsSecret + `"}}, "cipher": "aes-256-gcm", "secret": "x"`),
			"chained dialers are not supported"},
		"no address":    {side(`"endpoint": {"$type": "dial"}, "cipher": "aes-256-gcm", "secret": "x"`), "address is missing"},
		"no url":        {side(`"endpoint": {"$type": "websocket"}, "cipher": "aes-256-gcm", "secret": "x"`), "url is missing"},
		"url scheme":    {side(`"endpoint": ` + ws("ftp://ws.example.org"+wsPath) + `, "cipher": "aes-256-gcm", "secret": "x"`), "must be wss:// or ws://"},
		"url malformed": {side(`"endpoint": ` + ws("wss://ws.example.org:"+wsSecret+wsPath) + `, "cipher": "aes-256-gcm", "secret": "x"`), "websocket url is malformed"},
		"url port":      {side(`"endpoint": ` + ws("wss://ws.example.org:0"+wsPath) + `, "cipher": "aes-256-gcm", "secret": "x"`), "port must be"},
		"url no host":   {side(`"endpoint": ` + ws("wss://"+wsPath) + `, "cipher": "aes-256-gcm", "secret": "x"`), "no host"},
		"url userinfo":  {side(`"endpoint": ` + ws("wss://user:"+wsSecret+"@ws.example.org"+wsPath) + `, "cipher": "aes-256-gcm", "secret": "x"`), "credentials"},
		"nested ws": {side(`"endpoint": {"$type": "websocket", "url": "wss://ws.example.org` + wsPath + `", "endpoint": ` + ok + `}, "cipher": "aes-256-gcm", "secret": "x"`),
			`websocket endpoint "websocket" is not supported`},
		"inner endpoint": {side(`"endpoint": {"$type": "websocket", "url": "wss://ws.example.org` + wsPath + `", "endpoint": "` + wsSecret + `"}, "cipher": "aes-256-gcm", "secret": "x"`),
			"websocket endpoint: server must be host:port"},
		"split hosts": {tcpudp(ok, ws("wss://other.example.org/s3cr3t-ws-token/udp")), "same server host"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig(tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), wsSecret) || strings.Contains(err.Error(), "s3cr3t") {
				t.Fatalf("error quotes a secret or URL: %v", err)
			}
		})
	}
}
