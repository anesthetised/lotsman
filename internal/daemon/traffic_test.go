package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/traffic"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

// A client uploads through its tunnel, and the sampler shows it as up on its
// peer; the upstream interfaces' counters, faked, map rx to down.
func TestTraffic(t *testing.T) {
	h := newHarness(t)
	h.cfg.Listen = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), freeUDPPort(t)).String()
	var downNet *netstack.Net
	h.d.newTUN = func(name string, mtu int) (tun.Device, error) {
		addr := netip.MustParseAddr("10.0.0.1")
		if name == DownstreamInterface {
			addr = Gateway(h.cfg).Addr()
		}
		tn, network, err := netstack.CreateNetTUN([]netip.Addr{addr}, nil, mtu)
		if name == DownstreamInterface {
			downNet = network
		}
		return tn, err
	}
	h.d.interfaceCounters = func(iface string) (rx, tx uint64, err error) {
		if iface == "lm-up-nl" {
			return 5000, 700, nil
		}
		return 0, 0, errors.New("no such interface")
	}
	h.st.CreateUser("alice")
	peer, err := h.st.AddPeer("alice", store.DefaultDevice, "nl", h.cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	h.run(t)
	<-h.d.Ready()

	// A sink on the gateway: the client's bytes go up, only ACKs come down.
	target := netip.AddrPortFrom(Gateway(h.cfg).Addr(), 8080)
	ln, err := downNet.ListenTCPAddrPort(target)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(io.Discard, c) }()
		}
	}()
	conf, err := awgconf.Parse(strings.NewReader(ClientConfig(h.cfg, h.d.id, peer, h.d.ClientMTU()).String()))
	if err != nil {
		t.Fatal(err)
	}
	conf.Peers[0].Endpoint = h.cfg.Listen
	tn, clientNet, err := netstack.CreateNetTUN([]netip.Addr{peer.IP}, nil, conf.Interface.MTU)
	if err != nil {
		t.Fatal(err)
	}
	client := tunnel.New("client", tn, quiet)
	defer client.Close()
	if err := client.Configure(conf.UAPI()); err != nil {
		t.Fatal(err)
	}
	if err := client.Up(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := clientNet.DialContextTCPAddrPort(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const sent = 64_000
	if _, err := c.Write(make([]byte, sent)); err != nil {
		t.Fatal(err)
	}

	// The upload can take seconds through netstack under -race on a loaded
	// runner, and a rate lasts one sample after it ends, so wait longer than
	// eventually does and look for the rate in any snapshot on the way.
	var got traffic.Snapshot
	sawRate := false
	deadline := time.Now().Add(20 * time.Second)
	for {
		got = h.d.Traffic()
		p := got.Peers[peer.ID]
		sawRate = sawRate || p.UpRate > 0
		if p.UpBytes > sent && sawRate {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer traffic %+v after 20s: want more than %d bytes up and an up rate", p, sent)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p := got.Peers[peer.ID]; p.DownBytes >= p.UpBytes/4 {
		t.Errorf("peer traffic %+v: an upload should be mostly up", p)
	}
	if got.Total.UpBytes != got.Peers[peer.ID].UpBytes {
		t.Errorf("total %+v, peer %+v: one peer is the whole total", got.Total, got.Peers[peer.ID])
	}
	if u := got.Upstreams["nl"]; u.DownBytes != 5000 || u.UpBytes != 700 {
		t.Errorf("nl traffic = %+v, want rx as down and tx as up", u)
	}
	if _, ok := got.Upstreams["de"]; ok {
		t.Error("de traffic without counters")
	}
}

// BenchmarkTrafficSample is one sample of a gateway with 50 peers and two
// upstreams: the work the daemon adds every TrafficInterval.
func BenchmarkTrafficSample(b *testing.B) {
	h := newHarness(b)
	h.d.interfaceCounters = func(string) (uint64, uint64, error) { return 1, 1, nil }
	h.st.CreateUser("bench")
	for i := range 50 {
		if _, err := h.st.AddPeer("bench", fmt.Sprintf("d%d", i), "nl", h.cfg.Subnet); err != nil {
			b.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.d.Run(ctx) }()
	defer func() { cancel(); <-done }()
	<-h.d.Ready()
	s := &traffic.Sampler{Source: h.d}
	if got := len(s.Sample().Peers); got != 50 {
		b.Fatalf("sampled %d peers, want 50", got)
	}
	for b.Loop() {
		s.Sample()
	}
}
