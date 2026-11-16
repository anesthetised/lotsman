//go:build linux && integration

package linux

import (
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/nftables"
)

// TestVerifyRepairsRemovedFirewall removes parts of the firewall the way
// other tools do, then runs what a health round runs: Verify, and Route for
// the routed peer. Traffic must flow again and clients must again be kept off
// host services (issue #47). Nothing outside Lotsman's own objects and a test
// table is touched, since CI runs this on the runner's own network namespace.
func TestVerifyRepairsRemovedFirewall(t *testing.T) {
	d := packetNetwork(t)
	peer := netip.MustParseAddr("10.77.0.2")
	if err := d.Route(peer, "lm-up-a"); err != nil {
		t.Fatal(err)
	}
	checkPeerPackets(t, true)
	if repaired, err := d.Verify(); repaired || err != nil {
		t.Fatalf("Verify on an intact firewall = %v, %v", repaired, err)
	}

	// A host service the input chain must keep clients away from.
	ln, err := net.Listen("tcp4", "0.0.0.0:2223")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	reachesHost := func() bool {
		return exec.Command("ip", "netns", "exec", "lm-client", "timeout", "1",
			"bash", "-c", "echo x >/dev/tcp/10.77.0.1/2223").Run() == nil
	}
	if reachesHost() {
		t.Fatal("client reaches a host service before any flush")
	}

	nft := func(script string) func(t *testing.T) {
		return func(t *testing.T) {
			cmd := exec.Command("nft", "-f", "-")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("nft: %v: %s", err, out)
			}
		}
	}
	// What an iptables flush (such as a ufw reload) does to our accepts.
	removeForeignAccepts := func(t *testing.T) {
		if err := closeForeignForwardChains(&nftables.Conn{}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { exec.Command("nft", "delete", "table", "ip", "lmtest").Run() })

	for _, tc := range []struct {
		name   string
		remove func(t *testing.T)
	}{
		{"table deleted", nft("delete table inet lotsman")},
		{"peer set deleted", nft("flush chain inet lotsman forward\ndelete set inet lotsman authorized_peers")},
		// Like Docker starting after Lotsman: a forward chain that drops by default.
		{"foreign chain added", nft("table ip lmtest {\n chain forward {\n  type filter hook forward priority 0; policy drop;\n }\n}")},
		{"foreign accepts removed", removeForeignAccepts},
		// What "nft flush ruleset" does to our objects, while the foreign table returns.
		{"everything removed", func(t *testing.T) {
			removeForeignAccepts(t)
			nft("delete table inet lotsman")(t)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.remove(t)
			if repaired, err := d.Verify(); !repaired || err != nil {
				t.Errorf("Verify = %v, %v; want a repair", repaired, err)
			}
			// The daemon routes every routed peer again after a repair.
			if err := d.Route(peer, "lm-up-a"); err != nil {
				t.Fatal(err)
			}
			checkPeerPackets(t, true)
			if reachesHost() {
				t.Error("client reaches a host service after the repair")
			}
			if repaired, err := d.Verify(); repaired || err != nil {
				t.Errorf("Verify after the repair = %v, %v; want intact", repaired, err)
			}
		})
	}
}
