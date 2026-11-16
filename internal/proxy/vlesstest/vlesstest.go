// Package vlesstest provides an Xray VLESS + REALITY + XTLS Vision server on
// loopback for tests of VLESS upstreams.
//
// The server is Xray's own inbound, so tests show the client interoperates
// with Xray, not with a reimplementation. Xray's Vision server shares state
// between the two directions of a connection without synchronization, which
// the race detector reports on every run (upstream, xray-core v26.11.9); test
// files that run this server carry a "!race" build constraint.
package vlesstest

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	xreality "github.com/xtls/reality"
	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/app/router"
	xlog "github.com/xtls/xray-core/common/log"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/freedom"
	xvless "github.com/xtls/xray-core/proxy/vless"
	"github.com/xtls/xray-core/proxy/vless/inbound"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/reality"

	_ "github.com/xtls/xray-core/app/proxyman/inbound"
	_ "github.com/xtls/xray-core/app/proxyman/outbound"
)

// ServerName is the camouflage site's name: the sni of every link.
const ServerName = "camouflage.test"

// Server is a VLESS + REALITY + Vision server on a loopback TCP port.
type Server struct {
	addr      netip.AddrPort
	id        string
	publicKey []byte
	shortID   []byte
	cert      *x509.Certificate
	site      string // the camouflage site's address
	inst      *core.Instance
}

// NewServer starts a server and its camouflage site. Targets found in
// redirect, "ip:port" each, are dialed at the mapped address instead, TCP
// and UDP alike, and UDP replies from there come back as if from the
// requested target; the targets must differ in port. Other targets are
// dialed as requested, loopback and private addresses included. onCert, if
// set, sees the camouflage site's certificate before the server starts.
//
// Xray's log is one per process, and by default it writes every flow's
// details to stdout; NewServer replaces whatever handler it had with one
// that writes errors to stderr. A test that wants Xray's log elsewhere sets
// it after starting its servers.
func NewServer(t testing.TB, redirect map[string]string, onCert func(*x509.Certificate)) *Server {
	t.Helper()
	xlog.RegisterHandler(errorsOnly{})
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, site := camouflage(t)
	if onCert != nil {
		onCert(cert)
	}
	port := freePort(t)
	s := &Server{
		addr:      netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port),
		id:        uuid.NewString(),
		publicKey: key.PublicKey().Bytes(),
		shortID:   []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef},
		cert:      cert,
		site:      site,
	}
	// Xray's freedom refuses private addresses unless allowed.
	allow := []*freedom.FinalRuleConfig{{Action: freedom.RuleAction_Allow}}
	outbounds := []*core.OutboundHandlerConfig{{
		Tag:           "direct",
		ProxySettings: serial.ToTypedMessage(&freedom.Config{FinalRules: allow}),
	}}
	var rules []*router.RoutingRule
	for target, to := range redirect {
		from, dst := netip.MustParseAddrPort(target), netip.MustParseAddrPort(to)
		tag := "to-" + target
		outbounds = append(outbounds, &core.OutboundHandlerConfig{
			Tag: tag,
			ProxySettings: serial.ToTypedMessage(&freedom.Config{
				FinalRules: allow,
				DestinationOverride: &freedom.DestinationOverride{Server: &protocol.ServerEndpoint{
					Address: xnet.NewIPOrDomain(xnet.IPAddress(dst.Addr().AsSlice())),
					Port:    uint32(dst.Port()),
				}},
			}),
		})
		rules = append(rules, &router.RoutingRule{
			TargetTag: &router.RoutingRule_Tag{Tag: tag},
			PortList:  &xnet.PortList{Range: []*xnet.PortRange{xnet.SinglePortRange(xnet.Port(from.Port()))}},
		})
	}
	config := &core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&dispatcher.Config{}),
			serial.ToTypedMessage(&proxyman.InboundConfig{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
			serial.ToTypedMessage(&router.Config{Rule: rules}),
		},
		Inbound: []*core.InboundHandlerConfig{{
			ReceiverSettings: serial.ToTypedMessage(&proxyman.ReceiverConfig{
				PortList: &xnet.PortList{Range: []*xnet.PortRange{xnet.SinglePortRange(xnet.Port(port))}},
				Listen:   xnet.NewIPOrDomain(xnet.LocalHostIP),
				StreamSettings: &internet.StreamConfig{
					ProtocolName: "tcp",
					SecurityType: serial.GetMessageType(&reality.Config{}),
					SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(&reality.Config{
						Dest:        site,
						Type:        "tcp",
						ServerNames: []string{ServerName},
						PrivateKey:  key.Bytes(),
						ShortIds:    [][]byte{s.shortID},
					})},
				},
			}),
			ProxySettings: serial.ToTypedMessage(&inbound.Config{
				Users: []*protocol.User{{Account: serial.ToTypedMessage(&xvless.Account{
					Id:   s.id,
					Flow: xvless.XRV,
				})}},
			}),
		}},
		Outbound: outbounds,
	}
	inst, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inst.Close() })
	s.inst = inst
	return s
}

// Addr is where the server listens.
func (s *Server) Addr() netip.AddrPort { return s.addr }

// Link is a vless:// link for the server, as a panel exports it.
func (s *Server) Link() string {
	return s.LinkWith(url.Values{})
}

// LinkWith is Link with query parameters replaced or added; an empty value
// removes the parameter.
func (s *Server) LinkWith(params url.Values) string {
	q := url.Values{
		"type":       {"tcp"},
		"encryption": {"none"},
		"security":   {"reality"},
		"pbk":        {base64.RawURLEncoding.EncodeToString(s.publicKey)},
		"fp":         {"chrome"},
		"sni":        {ServerName},
		"sid":        {hex.EncodeToString(s.shortID)},
		"spx":        {"/"},
		"flow":       {xvless.XRV},
	}
	for k, v := range params {
		if v[0] == "" {
			q.Del(k)
		} else {
			q[k] = v
		}
	}
	return fmt.Sprintf("vless://%s@%s?%s#test", s.id, s.addr, q.Encode())
}

// ID is the server's one user ID, the secret in its links.
func (s *Server) ID() string { return s.id }

// Certificate is the camouflage site's certificate, which a client without
// the server's REALITY key receives.
func (s *Server) Certificate() *x509.Certificate { return s.cert }

// Close stops the server; open connections stay until they end.
func (s *Server) Close() { s.inst.Close() }

// WaitReady waits until each server has probed its camouflage site. REALITY
// learns the site's post-handshake records once per process and site,
// which takes 5 s against a site that keeps the connection open, as real
// sites do; authenticated flows wait for it. Starting every server first
// and then waiting runs the probes in parallel.
func WaitReady(t testing.TB, servers ...*Server) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for _, s := range servers {
		for alpn := range 3 {
			key := fmt.Sprintf("%s %s %d", s.site, ServerName, alpn)
			for {
				v, ok := xreality.GlobalPostHandshakeRecordsLens.Load(key)
				if _, probing := v.(bool); ok && !probing {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("REALITY did not finish probing %s", key)
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
}

// camouflage starts the REALITY target: a TLS 1.3 site whose handshake the
// server relays to clients that fail REALITY authentication, standing in
// for a real third-party site. It returns its certificate and address.
func camouflage(t testing.TB) (*x509.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: ServerName},
		DNSNames:              []string{ServerName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// A real HTTPS server, as REALITY expects of its target.
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "camouflage") }),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	return cert, ln.Addr().String()
}

// errorsOnly writes Xray's errors to stderr and drops the rest.
type errorsOnly struct{}

func (errorsOnly) Handle(msg xlog.Message) {
	if m, ok := msg.(*xlog.GeneralMessage); ok && m.Severity == xlog.Severity_Error {
		fmt.Fprintln(os.Stderr, "xray:", m)
	}
}

func (errorsOnly) Severity() xlog.Severity { return xlog.Severity_Error }

func freePort(t testing.TB) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}
