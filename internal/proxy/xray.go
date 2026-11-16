package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.getoutline.org/sdk/transport"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/app/policy"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common"
	xlog "github.com/xtls/xray-core/common/log"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/net/cnc"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/routing"
	xvless "github.com/xtls/xray-core/proxy/vless"
	"github.com/xtls/xray-core/proxy/vless/outbound"
	xtransport "github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/tcp"

	// The outbound handler manager that proxyman.OutboundConfig configures.
	_ "github.com/xtls/xray-core/app/proxyman/outbound"
)

// xrayIdle ends a flow that carried nothing either way for this long, and
// one direction this long after the other ended. Xray needs a limit; its
// defaults (5 min idle, and 1 s or 2 s after either direction ends) would
// cut idle SSH sessions and half-closed flows.
const xrayIdle = time.Hour

// xrayClient carries flows through one VLESS server with an Xray instance
// that has a single VLESS outbound and no inbound, DNS server or routing
// rules: the server and every target are IP addresses, so it never
// resolves a name. Several may run in one process.
type xrayClient struct {
	inst       *core.Instance
	dispatcher routing.Dispatcher
}

var (
	_ transport.StreamDialer   = (*xrayClient)(nil)
	_ transport.PacketListener = (*xrayClient)(nil)
)

func newXrayClient(k *vlessKey, server netip.AddrPort) (*xrayClient, error) {
	idle := &policy.Second{Value: uint32(xrayIdle / time.Second)}
	config := &core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&dispatcher.Config{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
			serial.ToTypedMessage(&policy.Config{Level: map[uint32]*policy.Policy{0: {
				Timeout: &policy.Policy_Timeout{
					ConnectionIdle: idle,
					UplinkOnly:     idle,
					DownlinkOnly:   idle,
				},
			}}}),
		},
		Outbound: []*core.OutboundHandlerConfig{{
			ProxySettings: serial.ToTypedMessage(&outbound.Config{
				Vnext: &protocol.ServerEndpoint{
					Address: xnet.NewIPOrDomain(xnet.IPAddress(server.Addr().Unmap().AsSlice())),
					Port:    uint32(server.Port()),
					User: &protocol.User{Account: serial.ToTypedMessage(&xvless.Account{
						Id:         k.id,
						Flow:       k.flow,
						Encryption: "none",
					})},
				},
			}),
			SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{
				StreamSettings: &internet.StreamConfig{
					ProtocolName: "tcp",
					TransportSettings: []*internet.TransportConfig{{
						ProtocolName: "tcp",
						Settings:     serial.ToTypedMessage(&tcp.Config{}),
					}},
					SecurityType: serial.GetMessageType(&reality.Config{}),
					SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(&reality.Config{
						Fingerprint:   k.fingerprint,
						ServerName:    k.serverName,
						PublicKey:     k.publicKey,
						ShortId:       k.shortID,
						Mldsa65Verify: k.mldsa65,
						SpiderX:       k.spiderX,
						SpiderY:       k.spiderY,
					})},
				},
			}),
		}},
	}
	inst, err := core.New(config)
	if err != nil {
		return nil, fmt.Errorf("xray: %w", err)
	}
	if err := inst.Start(); err != nil {
		inst.Close()
		return nil, fmt.Errorf("xray: start: %w", err)
	}
	return &xrayClient{inst: inst, dispatcher: inst.GetFeature(routing.DispatcherType()).(routing.Dispatcher)}, nil
}

// Close stops the instance. Flows already open keep their connections to
// the server until they end.
func (c *xrayClient) Close() error { return c.inst.Close() }

// DialStream starts a TCP flow to addr, an IP address and port. It returns
// before the server is reached: Xray connects in the background, so an
// unreachable server shows up as an error reading the flow, not here. The
// flow lives until ctx is done or the connection is closed.
func (c *xrayClient) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	target, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil, fmt.Errorf("target %s: want an IP address and port", addr)
	}
	// What core.Dial does: Xray's components find their instance in ctx.
	ctx, cancel := context.WithCancel(context.WithValue(ctx, core.XrayKey(1), c.inst))
	link, err := c.dispatcher.Dispatch(ctx, xnet.TCPDestination(xnet.IPAddress(target.Addr().Unmap().AsSlice()), xnet.Port(target.Port())))
	if err != nil {
		cancel()
		return nil, err
	}
	conn := cnc.NewConnection(cnc.ConnectionInputMulti(link.Writer), cnc.ConnectionOutputMulti(link.Reader))
	return &xrayStream{Conn: conn, link: link, cancel: cancel, remote: net.TCPAddrFromAddrPort(target)}, nil
}

type xrayStream struct {
	net.Conn
	link   *xtransport.Link
	cancel context.CancelFunc
	remote net.Addr
}

func (s *xrayStream) RemoteAddr() net.Addr { return s.remote }

// CloseWrite ends the uplink. VLESS has no half-close: the server stops
// reading, but the target sees no end of stream until the flow ends.
func (s *xrayStream) CloseWrite() error { return common.Close(s.link.Writer) }

func (s *xrayStream) CloseRead() error {
	common.Interrupt(s.link.Reader)
	return nil
}

func (s *xrayStream) Close() error {
	err := s.Conn.Close()
	s.cancel()
	return err
}

// ListenPacket returns a socket for one UDP flow, carried as XUDP inside a
// mux connection. Plain Vision refuses UDP to port 443 (QUIC); see
// flowVisionUDP443. The flow lives until ctx is done or the socket is
// closed.
func (c *xrayClient) ListenPacket(ctx context.Context) (net.PacketConn, error) {
	ctx, cancel := context.WithCancel(ctx)
	pc, err := core.DialUDP(ctx, c.inst)
	if err != nil {
		cancel()
		return nil, err
	}
	return &xrayPacketConn{PacketConn: pc, cancel: cancel}, nil
}

// xrayPacketConn also ends the flow's mux connection on Close, which Xray's
// own Close leaves open until it times out.
type xrayPacketConn struct {
	net.PacketConn
	cancel context.CancelFunc
}

func (p *xrayPacketConn) Close() error {
	err := p.PacketConn.Close()
	p.cancel()
	return err
}

// LogXray sends Xray's log, one per process, to log, replacing where it
// went before; until the first call Xray logs to stdout. Xray's info
// messages, several per flow including every failed one, are debug
// messages. Its errors and warnings, such as REALITY receiving a real
// certificate, keep their level once per minute for each kind of message
// and are debug messages otherwise, so a failure that repeats for every
// flow cannot flood the log.
func LogXray(log *slog.Logger) {
	xlog.RegisterHandler(&xrayLog{log: log, last: map[string]time.Time{}, now: time.Now})
}

// xrayLogEvery is how often a kind of Xray error or warning keeps its level.
const xrayLogEvery = time.Minute

// xrayLogKinds bounds the kinds of message remembered for xrayLogEvery.
const xrayLogKinds = 256

type xrayLog struct {
	log *slog.Logger
	now func() time.Time

	mu   sync.Mutex
	last map[string]time.Time // when each kind of message last kept its level
}

func (h *xrayLog) Handle(msg xlog.Message) {
	m, ok := msg.(*xlog.GeneralMessage)
	if !ok { // access and DNS records: per flow
		h.log.Debug(msg.String(), "component", "xray")
		return
	}
	text := strings.TrimSpace(fmt.Sprint(m.Content))
	level := slog.LevelDebug
	switch m.Severity {
	case xlog.Severity_Error:
		level = slog.LevelError
	case xlog.Severity_Warning:
		level = slog.LevelWarn
	}
	if level > slog.LevelDebug && !h.first(xrayLogKind(text)) {
		level = slog.LevelDebug
	}
	h.log.Log(context.Background(), level, text, "component", "xray")
}

// first reports whether kind is the first of its kind in xrayLogEvery.
func (h *xrayLog) first(kind string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if t, ok := h.last[kind]; ok && now.Sub(t) < xrayLogEvery {
		return false
	}
	if len(h.last) >= xrayLogKinds {
		for k, t := range h.last {
			if now.Sub(t) >= xrayLogEvery {
				delete(h.last, k)
			}
		}
		if len(h.last) >= xrayLogKinds {
			return false
		}
	}
	h.last[kind] = now
	return true
}

// xrayLogKind is what an Xray message says without its details: Xray
// writes "[session] component: message > cause > …", and the session and
// the causes, which carry addresses and system errors, vary per flow.
func xrayLogKind(text string) string {
	for strings.HasPrefix(text, "[") {
		_, rest, ok := strings.Cut(text, "] ")
		if !ok {
			break
		}
		text = rest
	}
	kind, _, _ := strings.Cut(text, " > ")
	return kind
}

// Severity lets Xray skip formatting what log would drop.
func (h *xrayLog) Severity() xlog.Severity {
	if h.log.Enabled(context.Background(), slog.LevelDebug) {
		return xlog.Severity_Debug
	}
	return xlog.Severity_Warning
}
