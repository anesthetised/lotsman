// Package proxytest provides a Shadowsocks server and simple targets on
// loopback for tests of proxy upstreams.
package proxytest

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

// Cipher is what Outline servers hand out.
const Cipher = "chacha20-ietf-poly1305"

// Server is a minimal Shadowsocks server on loopback, TCP and UDP on one
// port. It is built on go-shadowsocks2, an implementation independent of
// the Outline SDK client under test, so tests show interoperability, not
// symmetry.
type Server struct {
	tcp      *net.TCPListener
	udp      *net.UDPConn
	ciph     core.Cipher
	password string
	redirect map[string]string // target as requested → address actually dialed

	mu      sync.Mutex
	targets []string                  // "tcp <target>" or "udp <target>" per request, in order
	clients map[string]bool           // UDP client addresses seen, one per client socket
	conns   map[io.Closer]bool        // open target connections
	nat     map[string]net.PacketConn // UDP client address → socket toward the targets
	wg      sync.WaitGroup
}

// NewServer starts a server. Targets found in redirect are dialed at the
// mapped address instead, for TCP and UDP alike, and UDP replies from there
// are reported as coming from the requested target.
func NewServer(t testing.TB, password string, redirect map[string]string) *Server {
	t.Helper()
	ciph, err := core.PickCipher(Cipher, nil, password)
	if err != nil {
		t.Fatal(err)
	}
	ln, pc := listenPair(t)
	s := &Server{tcp: ln, udp: pc, ciph: ciph, password: password, redirect: redirect,
		clients: map[string]bool{}, conns: map[io.Closer]bool{}, nat: map[string]net.PacketConn{}}
	s.wg.Go(s.serveTCP)
	s.wg.Go(func() { s.serveUDP(ciph.PacketConn(pc)) })
	t.Cleanup(s.Close)
	return s
}

// listenPair listens for TCP and UDP on the same loopback port.
func listenPair(t testing.TB) (*net.TCPListener, *net.UDPConn) {
	t.Helper()
	for range 20 {
		pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: pc.LocalAddr().(*net.UDPAddr).Port})
		if err == nil {
			return ln, pc
		}
		pc.Close() // the TCP port is taken; try another
	}
	t.Fatal("no loopback port free for both TCP and UDP")
	return nil, nil
}

// Addr is where the server listens, for TCP and UDP.
func (s *Server) Addr() netip.AddrPort { return s.tcp.Addr().(*net.TCPAddr).AddrPort() }

// Key is an Outline-style access key for the server.
func (s *Server) Key() string {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte(Cipher + ":" + s.password))
	return "ss://" + userinfo + "@" + s.Addr().String() + "/?outline=1#test"
}

// Close stops the server and every connection it has open.
func (s *Server) Close() {
	s.tcp.Close()
	s.udp.Close()
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	for _, c := range s.nat {
		c.Close()
	}
	s.conns, s.nat = nil, nil // closed: track and serveUDP refuse new ones
	s.mu.Unlock()
	s.wg.Wait()
}

// Reset aborts every open client connection with a reset, as a server that
// fails does; the server keeps accepting new ones. Connections to targets
// stay, so no clean end of stream reaches a client first.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		if tc, ok := c.(*net.TCPConn); ok && tc.LocalAddr().String() == s.tcp.Addr().String() {
			tc.SetLinger(0)
			tc.Close()
			delete(s.conns, c)
		}
	}
}

// Seen lists the requests so far, "tcp <target>" or "udp <target>" each.
func (s *Server) Seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.targets)
}

// UDPClients counts the client sockets that sent UDP so far.
func (s *Server) UDPClients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

func (s *Server) dialAddr(target string) string {
	if to, ok := s.redirect[target]; ok {
		return to
	}
	return target
}

// track registers c for close; false if the server is already closed.
func (s *Server) track(c io.Closer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns == nil {
		return false
	}
	s.conns[c] = true
	return true
}

func (s *Server) serveTCP() {
	for {
		raw, err := s.tcp.AcceptTCP()
		if err != nil {
			return
		}
		s.wg.Go(func() {
			defer raw.Close()
			if s.track(raw) {
				s.serveStream(raw)
			}
		})
	}
}

// halfCloser is a client connection: TCP, or a WebSocket.
type halfCloser interface {
	net.Conn
	CloseWrite() error
}

// serveStream relays one Shadowsocks TCP connection to its target.
func (s *Server) serveStream(raw halfCloser) {
	c := s.ciph.StreamConn(raw)
	tgt, err := socks.ReadAddr(c)
	if err != nil {
		return // a wrong key: keep quiet, as outline-ss-server does
	}
	s.mu.Lock()
	s.targets = append(s.targets, "tcp "+tgt.String())
	s.mu.Unlock()
	rc, err := net.Dial("tcp", s.dialAddr(tgt.String()))
	if err != nil {
		return
	}
	defer rc.Close()
	if !s.track(rc) {
		return
	}
	done := make(chan struct{})
	go func() {
		io.Copy(rc, c)
		rc.(*net.TCPConn).CloseWrite()
		close(done)
	}()
	io.Copy(c, rc)
	raw.CloseWrite()
	<-done
}

// serveUDP relays Shadowsocks UDP from pc until it is closed.
func (s *Server) serveUDP(pc net.PacketConn) {
	buf := make([]byte, 64<<10)
	for {
		n, client, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // a packet that does not decrypt
		}
		tgt := socks.SplitAddr(buf[:n])
		if tgt == nil {
			continue
		}
		dst, err := net.ResolveUDPAddr("udp", s.dialAddr(tgt.String()))
		if err != nil {
			continue
		}
		s.mu.Lock()
		if s.nat == nil {
			s.mu.Unlock()
			return
		}
		s.targets = append(s.targets, "udp "+tgt.String())
		s.clients[client.String()] = true
		out, ok := s.nat[client.String()]
		if !ok {
			out, err = net.ListenPacket("udp4", ":0")
			if err != nil {
				s.mu.Unlock()
				continue
			}
			s.nat[client.String()] = out
			s.wg.Go(func() { s.replies(pc, out, client) })
		}
		s.mu.Unlock()
		out.WriteTo(buf[len(tgt):n], dst)
	}
}

// replies relays what targets send back to one UDP client.
func (s *Server) replies(pc, out net.PacketConn, client net.Addr) {
	buf := make([]byte, 64<<10)
	for {
		n, from, err := out.ReadFrom(buf)
		if err != nil {
			return
		}
		source := from.String()
		for target, to := range s.redirect {
			if to == source {
				source = target
			}
		}
		pc.WriteTo(append(socks.ParseAddr(source), buf[:n]...), client)
	}
}

// Serve runs handle on every connection ln accepts until the test ends.
func Serve(t testing.TB, ln net.Listener, handle func(net.Conn)) {
	t.Helper()
	var wg sync.WaitGroup
	t.Cleanup(func() {
		ln.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer c.Close()
				handle(c)
			})
		}
	})
}

// Echo returns everything until the client half-closes, then half-closes too.
func Echo(c net.Conn) {
	io.Copy(c, c)
	c.(*net.TCPConn).CloseWrite()
}

// Answer is the A record every DNS server here returns.
var Answer = netip.MustParseAddr("192.0.2.1")

// DNS starts a DNS server on loopback, UDP and TCP on one port, that
// answers every query with Answer.
func DNS(t testing.TB) netip.AddrPort {
	t.Helper()
	ln, pc := listenPair(t)
	ServeDNS(t, pc)
	Serve(t, ln, func(c net.Conn) {
		for {
			var size uint16
			if binary.Read(c, binary.BigEndian, &size) != nil {
				return
			}
			q := make([]byte, size)
			if _, err := io.ReadFull(c, q); err != nil || len(q) < 12 {
				return
			}
			c.Write(binary.BigEndian.AppendUint16(nil, uint16(len(answer(q)))))
			c.Write(answer(q))
		}
	})
	return ln.Addr().(*net.TCPAddr).AddrPort()
}

// ServeDNS answers every query on pc with Answer.
func ServeDNS(t testing.TB, pc net.PacketConn) {
	t.Helper()
	var wg sync.WaitGroup
	t.Cleanup(func() {
		pc.Close()
		wg.Wait()
	})
	wg.Go(func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n >= 12 {
				pc.WriteTo(answer(buf[:n]), from)
			}
		}
	})
}

func answer(query []byte) []byte {
	resp := slices.Clone(query)
	binary.BigEndian.PutUint16(resp[2:], 0x8180) // response, recursion desired and available
	binary.BigEndian.PutUint16(resp[6:], 1)      // one answer
	return append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 1)
}

// DNSQuery is an A query for lotsman.test.
func DNSQuery(id uint16) []byte {
	q := binary.BigEndian.AppendUint16(nil, id)
	q = append(q, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0) // recursion desired, one question
	for label := range strings.SplitSeq("lotsman.test", ".") {
		q = append(q, byte(len(label)))
		q = append(q, label...)
	}
	return append(q, 0, 0, 1, 0, 1) // root, type A, class IN
}

// ExchangeDNS sends one query on a UDP connection and checks the answer.
func ExchangeDNS(c net.Conn, id uint16) error {
	if _, err := c.Write(DNSQuery(id)); err != nil {
		return err
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil {
		return err
	}
	resp := buf[:n]
	if n < 12 || binary.BigEndian.Uint16(resp) != id || resp[2]&0x80 == 0 {
		return fmt.Errorf("bad response % x", resp)
	}
	if !bytes.HasSuffix(resp, Answer.AsSlice()) {
		return errors.New("response has no 192.0.2.1 answer")
	}
	return nil
}
