package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsHandshakeTimeout bounds connecting a WebSocket: TCP, TLS and the HTTP
// upgrade. A TCP flow's dial also ends at Options.DialTimeout; a UDP flow's
// has no other bound.
const wsHandshakeTimeout = 10 * time.Second

// wsRoots verifies wss:// servers; nil means the system's roots. Tests set
// their CA here, with verification on.
var wsRoots *x509.CertPool

// wsEndpoint is Outline's websocket endpoint: Shadowsocks carried over a
// WebSocket to url, as outline-ss-server's websocket-stream and
// websocket-packet listeners serve it. The URL's path is usually a secret,
// so the URL is never shown.
type wsEndpoint struct {
	url string // ws:// or wss://, without a fragment
}

// parseWSURL checks a websocket endpoint's URL as Outline's client takes it
// (outline-apps configregistry/config_websocket.go, parseWebsocketEndpoint):
// https:// and http:// stand for wss:// and ws://, the port defaults to 443
// or 80. It returns the endpoint and the URL's host:port. No error quotes
// the URL.
func parseWSURL(raw string) (*wsEndpoint, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, "", errors.New("websocket url is malformed")
	}
	port := "443"
	switch strings.ToLower(u.Scheme) {
	case "wss", "https":
		u.Scheme = "wss"
	case "ws", "http":
		u.Scheme, port = "ws", "80"
	default:
		return nil, "", errors.New("websocket url must be wss:// or ws://")
	}
	if u.User != nil {
		return nil, "", errors.New("websocket url must not carry credentials")
	}
	if u.Hostname() == "" {
		return nil, "", errors.New("websocket url has no host")
	}
	if u.Port() != "" {
		port = u.Port()
	}
	server := net.JoinHostPort(u.Hostname(), port)
	if err := splitServer(server); err != nil {
		return nil, "", fmt.Errorf("websocket url: %w", err)
	}
	u.Fragment, u.RawFragment = "", ""
	return &wsEndpoint{url: u.String()}, server, nil
}

// connect opens a WebSocket to the endpoint's URL over a TCP connection to
// addr, the server the daemon resolved: no DNS lookup here. TLS (server name
// and certificate check) and the HTTP Host header use the URL's host, as
// they would for a direct connection; no proxy from the environment is used.
// datagrams makes every Read return one message, for UDP.
func (e *wsEndpoint) connect(ctx context.Context, addr netip.AddrPort, datagrams bool) (*wsConn, error) {
	ctx, cancel := context.WithTimeout(ctx, wsHandshakeTimeout)
	defer cancel()
	var mu sync.Mutex
	var raw net.Conn
	d := websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var nd net.Dialer
			c, err := nd.DialContext(ctx, "tcp", addr.String())
			if err == nil {
				mu.Lock()
				raw = c
				mu.Unlock()
			}
			return c, err
		},
		TLSClientConfig: &tls.Config{RootCAs: wsRoots, MinVersion: tls.VersionTLS12},
	}
	// The dialer heeds ctx while connecting and in the TLS handshake, and
	// its deadline throughout, but not a cancellation during the upgrade.
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if raw != nil {
			raw.Close()
		}
	})
	ws, resp, err := d.DialContext(ctx, e.url, nil)
	if !stop() && err == nil {
		ws.Close()
		err = ctx.Err()
	}
	if err != nil {
		if errors.Is(err, websocket.ErrBadHandshake) && resp != nil {
			err = fmt.Errorf("websocket upgrade refused: HTTP status %d", resp.StatusCode)
		}
		return nil, err
	}
	// A close frame from the server ends what it sends, not what we send:
	// do not answer it with one, as the default handler would.
	ws.SetCloseHandler(func(int, string) error { return nil })
	return &wsConn{ws: ws, datagrams: datagrams}, nil
}

// wsConn is a WebSocket used as Outline's client and server use it
// (outline-sdk x/websocket, gorillaConn, which outline-ss-server serves
// with): every Write is one binary message, and a read returns the bytes of
// the messages in order, or for datagrams one message per Read, cut to the
// buffer. CloseWrite sends a close frame and keeps reading; the peer's close
// frame reads as the end of the stream. That is the half-close a TCP flow
// needs; the WebSocket closing handshake is never completed.
type wsConn struct {
	ws        *websocket.Conn
	datagrams bool

	readMu sync.Mutex
	msg    io.Reader // the rest of the message being read

	writeMu sync.Mutex
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
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
				return 0, io.EOF
			}
			return 0, err
		}
		if typ != websocket.BinaryMessage {
			return 0, errors.New("websocket: got a text message, want binary")
		}
		if c.datagrams {
			// The rest of a longer message is skipped by the next NextReader.
			n, err := io.ReadFull(r, b)
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				err = nil
			}
			return n, err
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

// CloseWrite sends a close frame; writing fails from then on.
func (c *wsConn) CloseWrite() error {
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	return c.ws.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
}

// CloseRead makes pending and future reads fail.
func (c *wsConn) CloseRead() error { return c.ws.SetReadDeadline(time.Now()) }

// Close drops the connection without a close frame: after a clean end both
// were sent already, and otherwise the flow failed.
func (c *wsConn) Close() error { return c.ws.Close() }

func (c *wsConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }

func (c *wsConn) SetDeadline(t time.Time) error {
	return errors.Join(c.ws.SetReadDeadline(t), c.ws.SetWriteDeadline(t))
}
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
