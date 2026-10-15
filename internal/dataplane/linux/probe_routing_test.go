//go:build linux && integration

package linux

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/anesthetised/lotsman/internal/dataplane"
)

func TestProbeRoutesWithOverlappingAddresses(t *testing.T) {
	for _, name := range []string{"lm0", "lm-up-a", "lm-up-b"} {
		dummy(t, name)
	}
	d := New()
	t.Cleanup(func() { d.Teardown() })
	source := netip.MustParseAddr("10.8.0.2")
	ups := []dataplane.Interface{
		{Name: "lm-up-a", Addr: netip.PrefixFrom(source, 32)},
		{Name: "lm-up-b", Addr: netip.PrefixFrom(source, 32)},
	}
	if err := d.Setup(dataplane.Interface{Name: "lm0", Addr: netip.MustParsePrefix("10.77.0.1/16")}, ups, 1380); err != nil {
		t.Fatal(err)
	}
	for _, up := range ups {
		table := d.tables[up.Name]
		found := false
		for _, r := range rulesAt(t, probeRulePriority) {
			if r.Table == table && r.OifName == up.Name && r.Src.IP.Equal(source.AsSlice()) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing source+oif probe rule for %s table %d", up.Name, table)
		}
		routes, err := netlink.RouteGetWithOptions(net.ParseIP("198.51.100.1"), &netlink.RouteGetOptions{SrcAddr: source.AsSlice(), Oif: up.Name})
		if err != nil || len(routes) != 1 {
			t.Fatalf("lookup on %s: %v, %v", up.Name, routes, err)
		}
		link, err := netlink.LinkByName(up.Name)
		if err != nil {
			t.Fatal(err)
		}
		if routes[0].Table != table || routes[0].LinkIndex != link.Attrs().Index {
			t.Errorf("lookup on %s = %v, want table %d on link %d", up.Name, routes[0], table, link.Attrs().Index)
		}
	}
	// A qualified foreign rule into our allocated table is still foreign when
	// its output selector names the other upstream.
	foreignRule(t, probeRulePriority, "192.0.2.1", d.tables["lm-up-a"], func(r *netlink.Rule) { r.OifName = "lm-up-b" })
	if err := d.Teardown(); err != nil {
		t.Fatal(err)
	}
	if got := rulesAt(t, probeRulePriority); len(got) != 1 || got[0].OifName != "lm-up-b" {
		t.Errorf("teardown did not preserve mismatched foreign rule: %v", got)
	}
}

func runUninstall(t *testing.T, stubs map[string]string) ([]byte, error) {
	t.Helper()
	dir := t.TempDir()
	manifest := filepath.Join(dir, "INSTALLED")
	if err := os.WriteFile(manifest, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// No package/path manifest entries and a stub systemctl keep the test to
	// actual routing cleanup inside the disposable container.
	if stubs == nil {
		stubs = map[string]string{}
	}
	if _, exists := stubs["systemctl"]; !exists {
		stubs["systemctl"] = "#!/bin/sh\nexit 0\n"
	}
	for name, script := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile("../../../deploy/uninstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	script = bytes.Replace(script, []byte("manifest=/etc/lotsman/INSTALLED"), []byte("manifest="+manifest), 1)
	cmd := exec.Command("sh")
	cmd.Stdin = bytes.NewReader(script)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	return cmd.CombinedOutput()
}

func TestProbeUninstallOwnership(t *testing.T) {
	for _, name := range []string{"lm-up-a", "lm-up-b", "fgn0"} {
		dummy(t, name)
	}
	foreignRoute(t, "lm-up-a", 1020, "0.0.0.0/0")
	foreignRule(t, probeRulePriority, "192.0.2.1", 1020, func(r *netlink.Rule) { r.OifName = "lm-up-b" })
	foreignRule(t, probeRulePriority, "192.0.2.2", 1021, func(r *netlink.Rule) { r.OifName = "fgn0" })
	foreign := rulesAt(t, probeRulePriority)
	foreignRule(t, probeRulePriority, "10.8.0.2", 1020, func(r *netlink.Rule) { r.OifName = "lm-up-a" })
	foreignRule(t, probeRulePriority, "10.9.0.2", 1022)
	foreignRule(t, probeRulePriority, "10.10.0.2", 1023, func(r *netlink.Rule) { r.OifName = "lm-up-gone" })
	if out, err := runUninstall(t, nil); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if got := rulesAt(t, probeRulePriority); !reflect.DeepEqual(got, foreign) {
		t.Errorf("uninstall rules = %v, want only foreign %v", got, foreign)
	}
}

func TestAmbiguousProbeCleanupPreservesState(t *testing.T) {
	for _, cleanup := range []string{"startup", "teardown", "uninstall"} {
		t.Run(cleanup, func(t *testing.T) {
			for _, name := range []string{"lm-up-a", "lm-up-b"} {
				dummy(t, name)
			}
			foreignRoute(t, "lm-up-a", 1050, "0.0.0.0/0")
			// Put the foreign qualified rule first: source-only deletion would
			// otherwise match it before reaching the legacy rule we own.
			foreignRule(t, probeRulePriority, "10.8.0.2", 1050, func(r *netlink.Rule) { r.OifName = "lm-up-b" })
			foreignRule(t, probeRulePriority, "10.8.0.2", 1050)
			// The generic helper's legacy RuleDel also has wildcard semantics.
			// Remove the qualified rule first when cleaning up this collision.
			t.Cleanup(func() {
				for _, r := range rulesAt(t, probeRulePriority) {
					if r.Table == 1050 && r.OifName != "" {
						netlink.RuleDel(&r)
					}
				}
				for _, r := range rulesAt(t, probeRulePriority) {
					if r.Table == 1050 {
						netlink.RuleDel(&r)
					}
				}
			})
			before := routingState(t)
			var err error
			switch cleanup {
			case "startup":
				err = removeLeftovers()
			case "teardown":
				d := New()
				d.tables["lm-up-a"] = 1050
				err = d.Teardown()
			case "uninstall":
				var out []byte
				out, err = runUninstall(t, nil)
				if !strings.Contains(string(out), "manual") {
					t.Errorf("uninstall did not explain incomplete cleanup: %s", out)
				}
			}
			if err == nil {
				t.Error("ambiguous cleanup succeeded")
			}
			if got := routingState(t); !reflect.DeepEqual(got, before) {
				t.Errorf("ambiguous cleanup changed routing\ngot %v\nwant %v", got, before)
			}
		})
	}
}

func TestProbeLeftoverOwnership(t *testing.T) {
	for _, name := range []string{"lm-up-a", "lm-up-b", "fgn0"} {
		dummy(t, name)
	}
	foreignRoute(t, "lm-up-a", 1020, "0.0.0.0/0")
	foreignRule(t, probeRulePriority, "192.0.2.1", 1020, func(r *netlink.Rule) { r.OifName = "lm-up-b" })
	foreignRule(t, probeRulePriority, "192.0.2.2", 1021, func(r *netlink.Rule) { r.OifName = "fgn0" })
	foreign := rulesAt(t, probeRulePriority)
	foreignRule(t, probeRulePriority, "10.8.0.2", 1020, func(r *netlink.Rule) { r.OifName = "lm-up-a" })
	foreignRule(t, probeRulePriority, "10.9.0.2", 1022) // legacy source-only
	foreignRule(t, probeRulePriority, "10.10.0.2", 1023, func(r *netlink.Rule) { r.OifName = "lm-up-gone" })
	if err := removeLeftovers(); err != nil {
		t.Fatal(err)
	}
	got := rulesAt(t, probeRulePriority)
	if len(got) != len(foreign) {
		t.Fatalf("probe rules after cleanup = %v, want only %v", got, foreign)
	}
	for i, r := range got {
		if r.OifName != foreign[i].OifName || !r.Src.IP.Equal(foreign[i].Src.IP) || r.Table != foreign[i].Table {
			t.Errorf("foreign probe rule changed: got %v, want %v", r, foreign[i])
		}
	}
}

func TestUninstallStopsIngressBeforeRemovingProtection(t *testing.T) {
	for _, failure := range []string{"none", "disable", "inspect"} {
		t.Run(failure, func(t *testing.T) {
			d := packetNetwork(t)
			if err := d.Route(netip.MustParseAddr("10.77.0.2"), "lm-up-a"); err != nil {
				t.Fatal(err)
			}
			packetCommand(t, "iptables", "-P", "FORWARD", "ACCEPT")
			dummy(t, "lm-up-b")
			foreignRoute(t, "lm-up-a", 1050, "0.0.0.0/0")
			foreignRule(t, probeRulePriority, "10.9.0.2", 1050, func(r *netlink.Rule) { r.OifName = "lm-up-b" })
			foreignRule(t, probeRulePriority, "10.9.0.2", 1050)
			t.Cleanup(func() {
				// Remove the qualified collision before any source-only deletion.
				packetCommand(t, "ip", "rule", "del", "priority", "4000", "from", "10.9.0.2", "oif", "lm-up-b", "lookup", "1050")
				packetCommand(t, "ip", "rule", "del", "priority", "4000", "from", "10.9.0.2", "lookup", "1050")
			})
			checkPeerPackets(t, true)
			routes := routingState(t)
			firewall := packetCommand(t, "nft", "list", "table", "inet", nftTable)
			boundary := filepath.Join(t.TempDir(), "nft-delete-boundary")
			ip, err := exec.LookPath("ip")
			if err != nil {
				t.Fatal(err)
			}
			nft, err := exec.LookPath("nft")
			if err != nil {
				t.Fatal(err)
			}
			stubs := map[string]string{
				"systemctl": "#!/bin/sh\nif [ \"$1\" = disable ]; then exit 1; fi\nexit 0\n",
				"nft":       fmt.Sprintf("#!/bin/sh\n%s -o link show dev lm0 > %s\nexec %s \"$@\"\n", ip, boundary, nft),
			}
			if failure != "none" {
				command := "link set dev lm0 down"
				if failure == "inspect" {
					command = "-o link show"
				}
				stubs["ip"] = fmt.Sprintf("#!/bin/sh\nif [ \"$*\" = \"%s\" ]; then exit 1; fi\nexec %s \"$@\"\n", command, ip)
			}
			out, err := runUninstall(t, stubs)
			if err == nil {
				t.Fatalf("uninstall should report incomplete cleanup: %s", out)
			}
			if failure != "none" {
				if got := routingState(t); !reflect.DeepEqual(got, routes) {
					t.Errorf("failed disable changed preserved routing: %v", got)
				}
				wantError := "disable downstream"
				if failure == "inspect" {
					wantError = "inspect downstream"
				}
				if !strings.Contains(string(out), wantError) {
					t.Errorf("missing %s error: %s", failure, out)
				}
				if _, err := os.Stat(boundary); !os.IsNotExist(err) {
					t.Error("nft cleanup ran after failed ingress disable")
				}
				if got := packetCommand(t, "nft", "list", "table", "inet", nftTable); got != firewall {
					t.Error("failed disable changed firewall")
				}
				checkPeerPackets(t, true)
				return
			}
			flags, err := os.ReadFile(boundary)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(flags), "state DOWN") {
				t.Errorf("downstream was not down at nft deletion: %s", flags)
			}
			link, err := netlink.LinkByName("lm0")
			if err != nil || link.Attrs().Flags&net.FlagUp != 0 {
				t.Errorf("retained downstream must be down: %v, %v", link, err)
			}
			checkPeerPackets(t, false)
		})
	}
}
