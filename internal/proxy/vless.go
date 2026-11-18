package proxy

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	xuuid "github.com/xtls/xray-core/common/uuid"
	xvless "github.com/xtls/xray-core/proxy/vless"
	xtls "github.com/xtls/xray-core/transport/internet/tls"
)

// The VLESS flows a link may ask for. Both are XTLS Vision; the -udp443
// variant also carries UDP to port 443 (QUIC), which plain Vision refuses so
// that browsers fall back to TCP, whose TLS Vision can hide.
const (
	flowVision       = xvless.XRV
	flowVisionUDP443 = xvless.XRV + "-udp443"
)

// vlessKey is a parsed vless:// link: a VLESS server with REALITY, either
// with XTLS Vision over TCP or without a flow over gRPC. Its REALITY fields
// are normalized as Xray's JSON loader does
// (infra/conf/transport_security.go, REALITYConfig.Build, client side),
// because Xray's client trusts them: a config without spiderY, for one,
// panics the process the first time the server presents a real certificate.
type vlessKey struct {
	server      string
	id          string  // the user ID, canonical; the secret
	network     string  // the transport: tcp or grpc
	flow        string  // tcp only
	serverName  string  // sni: the site REALITY impersonates
	fingerprint string  // fp: the uTLS ClientHello, lower case
	publicKey   []byte  // pbk: the server's X25519 key, 32 bytes
	shortID     []byte  // sid: 8 bytes, zero padded on the right
	spiderX     string  // spx, without the spider's own parameters
	spiderY     []int64 // the spider's ranges from spx, always 10 values
	mldsa65     []byte  // pqv: the server's ML-DSA-65 key, optional

	// gRPC only.
	serviceName string // the service, or a /service/stream path
	authority   string // the :authority header; empty for Xray's default
	multiMode   bool   // mode=multi
}

// ParseSpecKey reads a key as Spec carries it: a vless:// link, or what
// ParseConfig reads, which covers ss:// keys and dynamic keys' responses.
// VLESS links are kept apart from ParseConfig, which reads what Outline
// providers serve: a dynamic key never yields a VLESS upstream.
func ParseSpecKey(text string) (Key, error) {
	if isVLESS(text) {
		return ParseVLESS(text)
	}
	return ParseConfig(text)
}

// isVLESS reports whether s is a vless:// link rather than another key.
func isVLESS(s string) bool {
	scheme, _, ok := strings.Cut(strings.TrimSpace(s), "://")
	return ok && strings.EqualFold(scheme, "vless")
}

// vlessParams are the query parameters a link may carry with a value; any
// other must be absent or empty.
var vlessParams = []string{"security", "type", "headerType", "encryption", "flow", "sni", "fp", "pbk", "sid", "spx", "pqv",
	"serviceName", "mode", "authority", "alpn"}

// grpcParams are the parameters that apply to type=grpc only.
var grpcParams = []string{"serviceName", "mode", "authority"}

// ParseVLESS reads a vless:// share link (the format Xray clients and panels
// export):
//
//	vless://<uuid>@<host>:<port>?security=reality&pbk=…&sni=…&flow=xtls-rprx-vision[&type=tcp][&fp=chrome][&sid=…][&spx=/][&pqv=…]#name
//	vless://<uuid>@<host>:<port>?security=reality&pbk=…&sni=…&type=grpc[&serviceName=…][&mode=gun|multi][&authority=…][&fp=…][&sid=…][&spx=/][&pqv=…]#name
//
// Only REALITY is supported: over TCP with XTLS Vision, or over gRPC
// without a flow, as Vision runs over TCP only. alpn is accepted and
// ignored, as Xray does under REALITY, whose ClientHello carries the ALPN of
// its uTLS fingerprint. The fragment, a display name, is ignored, however it
// is written. No error quotes a value from the link; an unsupported
// parameter is named.
func ParseVLESS(s string) (Key, error) {
	s = strings.TrimSpace(s)
	if !isVLESS(s) {
		return nil, errors.New("vless key: want a vless:// link")
	}
	s, _, _ = strings.Cut(s, "#")
	u, err := url.Parse(s)
	if err != nil {
		// Not wrapped: url errors quote the URL.
		return nil, errors.New("vless key: malformed URL")
	}
	if u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("vless key: want vless://uuid@host:port?…")
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, errors.New("vless key: the user ID (uuid@) is missing")
	}
	if _, set := u.User.Password(); set {
		return nil, errors.New("vless key: want uuid@host:port, not user:password@")
	}
	if err := splitServer(u.Host); err != nil {
		return nil, fmt.Errorf("vless key: %w", err)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("vless key: malformed query")
	}
	for name, values := range q {
		if len(values) > 1 {
			return nil, fmt.Errorf("vless key: parameter %s given more than once", name)
		}
		if !slices.Contains(vlessParams, name) && values[0] != "" {
			return nil, fmt.Errorf("vless key: parameter %s is not supported", name)
		}
	}
	// Xray accepts a UUID or, as a shorthand, any 1 to 30 characters, which
	// it maps to a UUID.
	id, err := xuuid.ParseString(u.User.Username())
	if err != nil {
		return nil, errors.New("vless key: the user ID is not a UUID")
	}
	k := &vlessKey{server: u.Host, id: id.String()}
	switch q.Get("security") {
	case "reality":
	case "":
		return nil, errors.New("vless key: security is missing; only security=reality is supported")
	default:
		return nil, errors.New("vless key: only security=reality is supported")
	}
	// Xray reads the transport's name in any case.
	switch strings.ToLower(q.Get("type")) {
	case "", "tcp", "raw":
		k.network = "tcp"
	case "grpc":
		k.network = "grpc"
	default:
		return nil, errors.New("vless key: only type=tcp (raw) and type=grpc are supported")
	}
	switch q.Get("headerType") {
	case "", "none":
	default:
		return nil, errors.New("vless key: only headerType=none is supported")
	}
	switch q.Get("encryption") {
	case "", "none":
	default:
		return nil, errors.New("vless key: only encryption=none is supported")
	}
	if k.network == "grpc" {
		if err := k.grpc(q); err != nil {
			return nil, fmt.Errorf("vless key: %w", err)
		}
	} else {
		for _, p := range grpcParams {
			if q.Get(p) != "" {
				return nil, fmt.Errorf("vless key: %s applies to type=grpc only", p)
			}
		}
		switch k.flow = q.Get("flow"); k.flow {
		case flowVision, flowVisionUDP443:
		case "":
			return nil, fmt.Errorf("vless key: flow is missing; want %s or %s", flowVision, flowVisionUDP443)
		default:
			return nil, fmt.Errorf("vless key: unsupported flow; want %s or %s", flowVision, flowVisionUDP443)
		}
	}
	if k.serverName = q.Get("sni"); k.serverName == "" {
		return nil, errors.New("vless key: sni is missing")
	}
	if err := k.reality(q); err != nil {
		return nil, fmt.Errorf("vless key: %w", err)
	}
	return k, nil
}

// grpc reads the gRPC parameters. Xray's JSON loader takes any service
// name, including none and the /service/stream form, and any authority, so
// this does too; the link's mode is Xray's multiMode.
func (k *vlessKey) grpc(q url.Values) error {
	if q.Get("flow") != "" {
		return errors.New("flow must be empty for type=grpc: XTLS Vision runs over type=tcp only")
	}
	switch q.Get("mode") {
	case "", "gun":
	case "multi":
		k.multiMode = true
	default:
		return errors.New("mode: want gun or multi")
	}
	k.serviceName = q.Get("serviceName")
	k.authority = q.Get("authority")
	return nil
}

// reality checks and normalizes the REALITY parameters exactly as Xray's
// JSON loader does for a client.
func (k *vlessKey) reality(q url.Values) error {
	k.fingerprint = strings.ToLower(q.Get("fp"))
	if k.fingerprint == "" {
		k.fingerprint = "chrome" // what Xray uses when none is set
	}
	if k.fingerprint == "unsafe" || k.fingerprint == "hellogolang" {
		return errors.New("fp: this fingerprint is refused by Xray")
	}
	if xtls.GetFingerprint(k.fingerprint) == nil {
		return errors.New("fp: unknown fingerprint")
	}
	pbk := q.Get("pbk")
	if pbk == "" {
		return errors.New("pbk is missing")
	}
	var err error
	if k.publicKey, err = base64.RawURLEncoding.DecodeString(pbk); err != nil || len(k.publicKey) != 32 {
		return errors.New("pbk: want 32 bytes in unpadded base64url")
	}
	sid := q.Get("sid")
	if len(sid) > 16 {
		return errors.New("sid: at most 16 hex digits")
	}
	k.shortID = make([]byte, 8)
	if _, err := hex.Decode(k.shortID, []byte(sid)); err != nil {
		return errors.New("sid: want an even number of hex digits")
	}
	if pqv := q.Get("pqv"); pqv != "" {
		if k.mldsa65, err = base64.RawURLEncoding.DecodeString(pqv); err != nil || len(k.mldsa65) != 1952 {
			return errors.New("pqv: want an ML-DSA-65 public key, 1952 bytes in unpadded base64url")
		}
	}
	k.spiderX, k.spiderY, err = spider(q.Get("spx"))
	return err
}

// spider normalizes spiderX: the crawler that runs when the server presents
// a real certificate (a failed REALITY authentication, such as under a
// man-in-the-middle) starts at this path and indexes spiderY[0..9]
// unchecked. Query parameters p, c, t, i and r are ranges, "a" or "a-b",
// for padding, concurrency, times, interval and return; Xray moves them
// into spiderY and ignores numbers it cannot parse, and so does this.
func spider(spx string) (string, []int64, error) {
	if spx == "" {
		spx = "/"
	}
	if spx[0] != '/' {
		return "", nil, errors.New("spx: must start with /")
	}
	u, err := url.Parse(spx)
	if err != nil {
		// Xray ignores this error and then crashes on the nil URL.
		return "", nil, errors.New("spx: malformed path")
	}
	y := make([]int64, 10)
	q := u.Query()
	for i, p := range []string{"p", "c", "t", "i", "r"} {
		if v := q.Get(p); v != "" {
			lo, hi, ranged := strings.Cut(v, "-")
			if !ranged {
				hi = lo
			}
			// Xray splits on every "-" and reads the first two parts.
			hi, _, _ = strings.Cut(hi, "-")
			y[2*i], _ = strconv.ParseInt(lo, 10, 64)
			y[2*i+1], _ = strconv.ParseInt(hi, 10, 64)
		}
		q.Del(p)
	}
	u.RawQuery = q.Encode()
	return u.String(), y, nil
}

func (k *vlessKey) Server() string { return k.server }

// String names the transport, not the service name or authority, which a
// server may use as a path secret.
func (k *vlessKey) String() string {
	via := k.flow
	if k.network == "grpc" {
		via = "grpc"
		if k.multiMode {
			via += " multi"
		}
	}
	return fmt.Sprintf("vless reality %s, sni %s, fp %s", via, k.serverName, k.fingerprint)
}

// Dialers start an Xray instance that reaches the server at addr.
func (k *vlessKey) Dialers(addr netip.AddrPort) (Dialers, error) {
	c, err := newXrayClient(k, addr)
	if err != nil {
		return Dialers{}, err
	}
	return Dialers{Streams: c, Packets: c, Closer: c}, nil
}
