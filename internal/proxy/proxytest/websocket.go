package proxytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The paths a WebSocket front serves; like an Outline server's, they are
// the secret part of its URLs.
const (
	TCPPath = "/s3cr3t-ws-path/tcp"
	UDPPath = "/s3cr3t-ws-path/udp"
)

// Hostname is a name the WebSocket front's certificate is valid for, besides
// localhost. Nothing resolves it: clients connect to Addr.
const Hostname = "ws.lotsman.test"

// WebSocket fronts a Server the way outline-ss-server's websocket-stream and
// websocket-packet listeners do (cmd/outline-ss-server/main.go): an HTTP
// server on loopback, TLS unless plain, that upgrades a request for TCPPath
// to one Shadowsocks TCP connection and a request for UDPPath to a
// Shadowsocks UDP association, one datagram per binary message. Its
// connections behave like the Outline SDK's (x/websocket, gorillaConn),
// which that server uses.
type WebSocket struct {
	srv      *httptest.Server
	secure   bool
	password string
	roots    *x509.CertPool
	caPEM    []byte

	mu   sync.Mutex
	seen []Handshake
}

// Handshake is what a client sent when it opened a WebSocket.
type Handshake struct {
	Path string
	Host string // the HTTP Host header
	SNI  string // the TLS server name, empty without TLS
}

// WebSocket starts a WebSocket front for s until the test ends. With
// secure, it serves TLS with a certificate for Hostname and localhost from a
// CA of its own (Roots, CAPEM).
func (s *Server) WebSocket(t testing.TB, secure bool) *WebSocket {
	t.Helper()
	w := &WebSocket{secure: secure, password: s.password}
	var upgrader websocket.Upgrader
	accept := func(rw http.ResponseWriter, r *http.Request) *wsConn {
		h := Handshake{Path: r.URL.Path, Host: r.Host}
		if r.TLS != nil {
			h.SNI = r.TLS.ServerName
		}
		w.mu.Lock()
		w.seen = append(w.seen, h)
		w.mu.Unlock()
		ws, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			return nil
		}
		c := newWSConn(ws)
		if !s.track(c) {
			c.Close()
			return nil
		}
		return c
	}
	mux := http.NewServeMux()
	mux.HandleFunc(TCPPath, func(rw http.ResponseWriter, r *http.Request) {
		if c := accept(rw, r); c != nil {
			defer c.Close()
			s.serveStream(c)
		}
	})
	mux.HandleFunc(UDPPath, func(rw http.ResponseWriter, r *http.Request) {
		if c := accept(rw, r); c != nil {
			defer c.Close()
			s.serveUDP(s.ciph.PacketConn(&wsPacketConn{c: c, addr: wsAddr("ws " + r.RemoteAddr)}))
		}
	})
	w.srv = httptest.NewUnstartedServer(mux)
	w.srv.Config.ErrorLog = log.New(io.Discard, "", 0) // untrusted-CA tests fail handshakes on purpose
	if secure {
		cert, roots, caPEM := testCert(t)
		w.roots, w.caPEM = roots, caPEM
		w.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
		w.srv.StartTLS()
	} else {
		w.srv.Start()
	}
	t.Cleanup(w.srv.Close)
	return w
}

// Addr is where the front listens.
func (w *WebSocket) Addr() netip.AddrPort {
	return w.srv.Listener.Addr().(*net.TCPAddr).AddrPort()
}

// Roots trusts the front's certificate; nil without TLS.
func (w *WebSocket) Roots() *x509.CertPool { return w.roots }

// CAPEM is the front's CA certificate in PEM; nil without TLS.
func (w *WebSocket) CAPEM() []byte { return w.caPEM }

// URL is the WebSocket URL of path at host, with the front's port.
func (w *WebSocket) URL(host, path string) string {
	scheme := "ws"
	if w.secure {
		scheme = "wss"
	}
	return fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(host, fmt.Sprint(w.Addr().Port())), path)
}

// Config is an Outline config, as a provider would serve it from an
// ssconf:// URL, that carries TCP and UDP over the front's WebSockets at
// host, which Hostname or localhost must be for TLS.
func (w *WebSocket) Config(host string) string {
	side := func(path string) string {
		return fmt.Sprintf(`
    $type: shadowsocks
    endpoint:
      $type: websocket
      url: %s
    cipher: %s
    secret: %s`, w.URL(host, path), Cipher, w.password)
	}
	return "transport:\n  $type: tcpudp\n  tcp:" + side(TCPPath) + "\n  udp:" + side(UDPPath) + "\n"
}

// Handshakes lists the WebSocket requests so far, in order.
func (w *WebSocket) Handshakes() []Handshake {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.seen)
}

// testCert makes a CA and a certificate it signed for Hostname and
// localhost.
func testCert(t testing.TB) (tls.Certificate, *x509.CertPool, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "lotsman test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: Hostname},
		DNSNames:     []string{Hostname, "localhost"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return cert, roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

// wsConn is the server's side of a WebSocket, after the Outline SDK's
// gorillaConn: every Write is one binary message, reads return the
// messages' bytes in order, and a close frame from the peer reads as the end
// of the stream, without one sent back. CloseWrite sends a close frame and
// keeps reading: half-close.
type wsConn struct {
	ws      *websocket.Conn
	readMu  sync.Mutex
	msg     io.Reader
	writeMu sync.Mutex
}

func newWSConn(ws *websocket.Conn) *wsConn {
	ws.SetCloseHandler(func(int, string) error { return nil })
	return &wsConn{ws: ws}
}

func (c *wsConn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		if c.msg != nil {
			n, err := c.msg.Read(b)
			if err == io.EOF {
				c.msg, err = nil, nil
				if n == 0 {
					continue
				}
			}
			return n, err
		}
		typ, r, err := c.ws.NextReader()
		if _, closed := err.(*websocket.CloseError); closed {
			return 0, io.EOF // whatever the code, as the SDK reads it
		}
		if err != nil {
			return 0, err
		}
		if typ != websocket.BinaryMessage {
			return 0, fmt.Errorf("message type %d is not binary", typ)
		}
		c.msg = r
	}
}

func (c *wsConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *wsConn) CloseWrite() error {
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	return c.ws.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
}

func (c *wsConn) Close() error {
	c.CloseWrite()
	return c.ws.Close()
}

func (c *wsConn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr               { return c.ws.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error      { return c.ws.UnderlyingConn().SetDeadline(t) }
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }

// wsPacketConn is a UDP association over a WebSocket: one datagram per
// message, all from and to one client address.
type wsPacketConn struct {
	c    *wsConn
	addr net.Addr
}

func (p *wsPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	_, r, err := p.c.ws.NextReader()
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", net.ErrClosed, err) // ends serveUDP
	}
	n, err := io.ReadFull(r, b)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		err = nil
	}
	return n, p.addr, err
}

func (p *wsPacketConn) WriteTo(b []byte, _ net.Addr) (int, error) { return p.c.Write(b) }
func (p *wsPacketConn) Close() error                              { return p.c.Close() }
func (p *wsPacketConn) LocalAddr() net.Addr                       { return p.c.LocalAddr() }
func (p *wsPacketConn) SetDeadline(t time.Time) error             { return p.c.SetDeadline(t) }
func (p *wsPacketConn) SetReadDeadline(t time.Time) error         { return p.c.SetReadDeadline(t) }
func (p *wsPacketConn) SetWriteDeadline(t time.Time) error        { return p.c.SetWriteDeadline(t) }

// wsAddr names a UDP association's client, distinct from UDP addresses.
type wsAddr string

func (a wsAddr) Network() string { return "ws" }
func (a wsAddr) String() string  { return string(a) }
