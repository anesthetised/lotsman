package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/policy"
	"github.com/anesthetised/lotsman/internal/store"
)

// capture runs the CLI and returns what it printed to stdout.
func capture(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := run(args)
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String(), err
}

// mustRun is capture for commands that must succeed.
func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := capture(t, args...)
	if err != nil {
		t.Fatalf("lotsman %q: %v", args, err)
	}
	return out
}

// captureStderr returns what f wrote to stderr.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	f()
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// tableRow returns the fields of the first line of a CLI table whose first
// column is key, or nil.
func tableRow(table, key string) []string {
	for line := range strings.Lines(table) {
		if f := strings.Fields(line); len(f) > 0 && f[0] == key {
			return f
		}
	}
	return nil
}

func writeProviderConf(t *testing.T, dir, name string) string {
	t.Helper()
	key, _ := awgconf.GeneratePrivateKey()
	peer, _ := awgconf.GeneratePrivateKey()
	conf := &awgconf.Config{
		Interface: awgconf.Interface{PrivateKey: key, Addresses: []netip.Prefix{netip.MustParsePrefix("10.8.0.2/32")}, MTU: 1400, Params: awgconf.Params{S4: 8}},
		Peers:     []awgconf.Peer{{PublicKey: peer.Public(), Endpoint: "192.0.2.1:51820", AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}},
	}
	path := filepath.Join(dir, name+".conf")
	os.WriteFile(path, []byte(conf.String()), 0o600)
	return path
}

func testConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	up := writeProviderConf(t, dir, "nl")
	path := filepath.Join(dir, "lotsman.yaml")
	os.WriteFile(path, []byte(`
endpoint: 203.0.113.10:51820
state_dir: `+filepath.Join(dir, "state")+`
upstreams: [{name: nl, conf: `+up+`, geo: NL, provider: acme}]
`), 0o600)
	return path
}

func TestUpstreamExpiryCommand(t *testing.T) {
	config := testConfig(t)
	for _, tc := range []struct {
		args []string
		out  string
	}{
		{[]string{"nl"}, "-\n"},
		{[]string{"nl", "2030-02-03T12:34:56+02:00"}, ""},
		{[]string{"nl"}, "2030-02-03T10:34:56Z\n"},
		{[]string{"nl", "2031-04-05T06:07:08Z"}, ""},
		{[]string{"nl"}, "2031-04-05T06:07:08Z\n"},
		{[]string{"nl", "clear"}, ""},
		{[]string{"nl"}, "-\n"},
	} {
		got, err := capture(t, append([]string{"-config", config, "upstream", "expiry"}, tc.args...)...)
		if err != nil || got != tc.out {
			t.Fatalf("upstream expiry %v = %q, %v; want %q", tc.args, got, err, tc.out)
		}
	}
	for _, args := range [][]string{{"unknown", "2030-02-03T12:34:56Z"}, {"nl", "2030-02-03"}} {
		if _, err := capture(t, append([]string{"-config", config, "upstream", "expiry"}, args...)...); err == nil {
			t.Fatalf("upstream expiry %v should fail", args)
		}
	}
}

func TestConfigCheckRejectsUnsafeUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, old, replacement, wantError string
	}{
		{"negative MTU", "MTU = 1400", "MTU = -1", "MTU minus S4 must be at least 68"},
		{"padding exhausts MTU", "S4 = 8", "S4 = 1333", "MTU minus S4 must be at least 68"},
		{"oversized wire MTU", "MTU = 1400", "MTU = 65468", "MTU plus S4 and transport overhead"},
		{"oversized IPv4 junk", "S4 = 8", "S4 = 8\nJc = 1\nJmin = 65508\nJmax = 65508", "junk packet size must not exceed 65507"},
		{"inverted junk", "S4 = 8", "S4 = 8\nJc = 1\nJmin = 70\nJmax = 40", "Jmin must not exceed Jmax"},
		{"missing peer key", "PublicKey =", "# PublicKey =", "PublicKey is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := testConfig(t)
			providerPath := filepath.Join(filepath.Dir(path), "nl.conf")
			provider, err := os.ReadFile(providerPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(provider), tc.old) {
				t.Fatalf("fixture does not contain %q", tc.old)
			}
			if err := os.WriteFile(providerPath, []byte(strings.Replace(string(provider), tc.old, tc.replacement, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := capture(t, "-config", path, "config", "check")
			if err == nil || !strings.Contains(err.Error(), tc.wantError) || !strings.Contains(err.Error(), "upstream nl:") {
				t.Fatalf("config check error = %v, want upstream nl and %q", err, tc.wantError)
			}
			if strings.Contains(out, "config OK") {
				t.Fatalf("invalid config reported OK: %s", out)
			}
		})
	}
}

func TestCLI(t *testing.T) {
	cfg := testConfig(t)

	out, err := capture(t, "-config", cfg, "config", "check")
	if err != nil || !strings.Contains(out, "config OK") || !strings.Contains(out, "lm-up-nl  kind awg  geo NL  provider acme") {
		t.Fatalf("config check: %v\n%s", err, out)
	}

	out, err = capture(t, "-config", cfg, "device", "add", "alice/laptop")
	if err != nil {
		t.Fatal(err)
	}
	client, err := awgconf.Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("device add output is not a config: %v\n%s", err, out)
	}
	if client.Interface.MTU != 1392 || client.Interface.Addresses[0].String() != "10.77.0.2/32" || client.Peers[0].Endpoint != "203.0.113.10:51820" {
		t.Errorf("client config = %+v", client)
	}
	if _, err := capture(t, "-config", cfg, "device", "add", "alice/laptop"); err == nil {
		t.Error("duplicate device accepted")
	}
	again, err := capture(t, "-config", cfg, "device", "show", "alice/laptop")
	if err != nil || again != out {
		t.Errorf("device show differs from device add: %v\n%s", err, again)
	}

	// A second device gets its own key and address.
	phone, err := capture(t, "-config", cfg, "device", "add", "alice/phone", "NL:acme")
	if err != nil {
		t.Fatal(err)
	}
	if phone == out {
		t.Error("phone config identical to the laptop's config")
	}
	list, err := capture(t, "-config", cfg, "user", "list")
	row := tableRow(list, "alice") // alice laptop 10.77.0.2 - - <created> any
	if err != nil || strings.Count(list, "alice") != 2 || !strings.Contains(list, "10.77.0.3") ||
		len(row) != 7 || !slices.Equal(row[:5], []string{"alice", "laptop", "10.77.0.2", "-", "-"}) || row[6] != "any" {
		t.Errorf("user list: %v\n%s", err, list)
	}

	if _, err := capture(t, "-config", cfg, "device", "rm", "alice/phone"); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(t, "-config", cfg, "device", "rm", "alice/phone"); err == nil {
		t.Error("removing a missing device succeeded")
	}
	// Without devices the user stays, and the list still shows them.
	if _, err := capture(t, "-config", cfg, "device", "rm", "alice/laptop"); err != nil {
		t.Fatal(err)
	}
	list, _ = capture(t, "-config", cfg, "user", "list")
	if strings.Count(list, "alice") != 1 || strings.Contains(list, "laptop") {
		t.Errorf("after removing the last device:\n%s", list)
	}
	if _, err := capture(t, "-config", cfg, "user", "rm", "alice"); err != nil {
		t.Fatal(err)
	}
	list, _ = capture(t, "-config", cfg, "user", "list")
	if strings.Contains(list, "alice") {
		t.Errorf("user survived rm:\n%s", list)
	}
	if _, err := capture(t, "-config", cfg, "user", "rm", "alice"); err == nil {
		t.Error("removing a missing user succeeded")
	}

	// -qr draws on stderr; stdout must remain a parseable config.
	r, w, _ := os.Pipe()
	oldErr := os.Stderr
	os.Stderr = w
	withQR, err := capture(t, "-config", cfg, "device", "add", "bob/phone", "-qr")
	w.Close()
	os.Stderr = oldErr
	var qr bytes.Buffer
	io.Copy(&qr, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := awgconf.Parse(strings.NewReader(withQR)); err != nil {
		t.Errorf("stdout with -qr is not a clean config: %v", err)
	}
	if !strings.Contains(qr.String(), "█") {
		t.Errorf("no QR code on stderr:\n%s", qr.String())
	}

	if _, err := capture(t, "-config", cfg, "upstream", "status"); err == nil {
		t.Error("upstream status without a daemon should fail")
	}
	if _, err := capture(t, "-config", cfg, "bogus"); err == nil {
		t.Error("unknown command accepted")
	}
	if _, err := capture(t); err == nil {
		t.Error("no command accepted")
	}
}

// Go's flag package stops at the first positional argument, so anything after
// it must be rejected rather than silently ignored (and then acted on).
func TestCLIRejectsTrailingArgs(t *testing.T) {
	cfg := testConfig(t)
	for _, device := range []string{"alice/laptop", "alice/phone"} {
		mustRun(t, "-config", cfg, "device", "add", device)
	}
	before := userState(t, cfg)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"user", "rm", "alice", "phone"}, "unexpected argument"},
		{[]string{"user", "rm", "alice", "-device", "phone"}, "unexpected argument"},
		{[]string{"user", "add", "bob", "phone"}, "unexpected argument"},
		{[]string{"user", "rights", "alice", "phone", "-edit-routes=false"}, "unexpected argument"},
		{[]string{"user", "link", "alice", "extra"}, "unexpected argument"},
		{[]string{"device", "show", "alice/phone", "extra"}, "unexpected argument"},
		{[]string{"device", "rm", "alice/phone", "extra"}, "unexpected argument"},
		{[]string{"device", "add", "bob/phone", "NL", "-fallback"}, "flags go before the countries"},
		{[]string{"device", "route", "alice/phone", "NL", "-fastest"}, "flags go before the countries"},
	} {
		_, err := capture(t, append([]string{"-config", cfg}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
		if after := userState(t, cfg); after != before {
			t.Errorf("%v changed the store:\nbefore %s\nafter  %s", tc.args, before, after)
		}
	}
}

func TestCLIRejectsArgumentsOnArgumentlessCommands(t *testing.T) {
	cfg := testConfig(t)
	for _, args := range [][]string{
		{"version", "extra"},
		{"serve", "extra"},
		{"serve", "-config", cfg},
		{"config", "check", "extra"},
		{"user", "list", "alice"},
		{"upstream", "status", "-json"},
		{"web", "extra"},
	} {
		_, err := capture(t, append([]string{"-config", cfg}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
			t.Errorf("%v: err = %v, want unexpected argument", args, err)
		}
	}
	if _, err := capture(t, "-config", cfg, "config", "check"); err != nil {
		t.Errorf("config check: %v", err)
	}
	if _, err := capture(t, "-config", cfg, "user", "list"); err != nil {
		t.Errorf("user list: %v", err)
	}
}

func TestCLIRejectsSubnetHostBits(t *testing.T) {
	path := testConfig(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := capture(t, "-config", path, "device", "add", "alice/phone")
	if err != nil {
		t.Fatal(err)
	}
	e, err := openEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.store.Close()
	peers, err := e.store.ListPeers()
	if err != nil {
		t.Fatal(err)
	}
	invalid := append(append([]byte(nil), original...), []byte("subnet: 10.77.0.255/24\n")...)
	if err := os.WriteFile(path, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "check"}, {"device", "add", "bob/phone"}} {
		out, err := capture(t, append([]string{"-config", path}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "subnet: host bits must be zero") || out != "" {
			t.Errorf("%v: output = %q, error = %v; want subnet host-bits error", args, out, err)
		}
	}
	if after, err := e.store.ListPeers(); err != nil || !reflect.DeepEqual(after, peers) {
		t.Errorf("rejected config changed stored peers: %+v, %v", after, err)
	}
	if users, err := e.store.ListUsers(); err != nil || len(users) != 1 || users[0].Name != "alice" {
		t.Errorf("rejected config created a user: %+v, %v", users, err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := capture(t, "-config", path, "device", "show", "alice/phone")
	if err != nil || after != before {
		t.Errorf("canonical config no longer reproduces stored peer: %v", err)
	}
}

func TestCanonicalSubnetGateway(t *testing.T) {
	cfg, err := config.Load(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ subnet, gateway string }{
		{"0.0.0.0/0", "0.0.0.1/0"},
		{"10.77.0.0/16", "10.77.0.1/16"},
		{"10.77.42.0/24", "10.77.42.1/24"},
		{"255.255.255.0/24", "255.255.255.1/24"},
	} {
		t.Run(tc.subnet, func(t *testing.T) {
			cfg.Subnet = netip.MustParsePrefix(tc.subnet)
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			got := daemon.Gateway(cfg)
			if got.String() != tc.gateway || !cfg.Subnet.Contains(got.Addr()) {
				t.Errorf("Gateway = %s, want %s inside %s", got, tc.gateway, cfg.Subnet)
			}
		})
	}
}

func TestCLIRejectsInvalidEndpoint(t *testing.T) {
	path := testConfig(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := capture(t, "-config", path, "device", "add", "alice/phone")
	if err != nil {
		t.Fatal(err)
	}
	e, err := openEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.store.Close()
	peers, err := e.store.ListPeers()
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{":51820", "vpn.example.org:51820", "203.0.113.10:banana", "203.0.113.10:0", "203.0.113.10:65536"} {
		t.Run(endpoint, func(t *testing.T) {
			invalid := strings.Replace(string(original), "endpoint: 203.0.113.10:51820", "endpoint: '"+endpoint+"'", 1)
			if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"config", "check"}, {"device", "add", "bob/phone"}, {"device", "show", "alice/phone"}} {
				out, err := capture(t, append([]string{"-config", path}, args...)...)
				if err == nil || !strings.HasPrefix(err.Error(), "endpoint:") || out != "" {
					t.Errorf("%v: printed output = %v, error = %v; want endpoint error without output", args, out != "", err)
				}
			}
			if after, err := e.store.ListPeers(); err != nil || !reflect.DeepEqual(after, peers) {
				t.Errorf("invalid endpoint changed stored peers: %v", err)
			}
			if users, err := e.store.ListUsers(); err != nil || len(users) != 1 || users[0].Name != "alice" {
				t.Errorf("invalid endpoint created a user: %v", err)
			}
		})
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := capture(t, "-config", path, "device", "show", "alice/phone")
	if err != nil || after != before {
		t.Errorf("valid endpoint no longer reproduces stored peer: %v", err)
	}
}

// Client configs depend on provider MTU and S4 only, so device commands must work
// while DNS fails. config check still resolves provider endpoints.
func TestUserCommandsWithoutDNS(t *testing.T) {
	path := testConfig(t)
	providerPath := filepath.Join(filepath.Dir(path), "nl.conf")
	provider, err := os.ReadFile(providerPath)
	if err != nil {
		t.Fatal(err)
	}
	hostname := strings.Replace(string(provider), "192.0.2.1:51820", "vpn.provider.test:51820", 1)
	if err := os.WriteFile(providerPath, []byte(hostname), 0o600); err != nil {
		t.Fatal(err)
	}
	// The Go resolver sends queries through Dial, so none leaves the host.
	r := net.DefaultResolver
	preferGo, dial := r.PreferGo, r.Dial
	r.PreferGo = true
	r.Dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("DNS disabled by test")
	}
	t.Cleanup(func() { r.PreferGo, r.Dial = preferGo, dial })

	if _, err := capture(t, "-config", path, "config", "check"); err == nil || !strings.Contains(err.Error(), "resolve endpoint") {
		t.Fatalf("config check error = %v, want a resolve error", err)
	}
	added, err := capture(t, "-config", path, "device", "add", "alice/phone")
	if err != nil {
		t.Fatal(err)
	}
	if client, err := awgconf.Parse(strings.NewReader(added)); err != nil || client.Interface.MTU != 1392 {
		t.Errorf("device add output is not a config with MTU 1392: %v\n%s", err, added)
	}
	shown, err := capture(t, "-config", path, "device", "show", "alice/phone")
	if err != nil || shown != added {
		t.Errorf("device show: %v\n%s", err, shown)
	}
}

// userState lists every user and peer; peers carry their keys and addresses,
// so any partial write shows up as a difference.
func userState(t *testing.T, cfg string) string {
	t.Helper()
	e, err := openEnv(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.store.Close()
	users, err := e.store.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	peers, err := e.store.ListPeers()
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%+v\n%+v", users, peers)
}

// A device is always named in full: an empty half, typically an unset
// shell variable, must not reach the store.
func TestDeviceNeedsUserAndDevice(t *testing.T) {
	cfg := testConfig(t)
	mustRun(t, "-config", cfg, "device", "add", "alice/phone")
	before := userState(t, cfg)
	for _, cmd := range []string{"add", "route", "show", "rm"} {
		for _, name := range []string{"", "alice", "alice/", "/phone", "-qr"} {
			_, err := capture(t, "-config", cfg, "device", cmd, name)
			if err == nil || !strings.Contains(err.Error(), "USER/DEVICE") {
				t.Errorf("device %s %q: err = %v, want a USER/DEVICE error", cmd, name, err)
			}
		}
	}
	if after := userState(t, cfg); after != before {
		t.Fatalf("a rejected command changed the store:\nbefore %s\nafter  %s", before, after)
	}
	// A device name may hold a slash; the user's may not.
	mustRun(t, "-config", cfg, "device", "add", "alice/work/laptop")
	if _, err := capture(t, "-config", cfg, "device", "show", "alice/work/laptop"); err != nil {
		t.Errorf("device show of a device with a slash: %v", err)
	}
	if _, err := capture(t, "-config", cfg, "user", "add", "bob/x"); err == nil {
		t.Error("user add accepted a name with a slash")
	}
}

// A rejected `device add` must leave users and peers exactly as they were.
func TestDeviceAddFailureLeavesNothingBehind(t *testing.T) {
	t.Run("unreadable provider file", func(t *testing.T) {
		cfg := testConfig(t)
		mustRun(t, "-config", cfg, "device", "add", "alice/phone")
		before := userState(t, cfg)
		if err := os.Remove(filepath.Join(filepath.Dir(cfg), "nl.conf")); err != nil {
			t.Fatal(err)
		}
		for _, device := range []string{
			"alice/laptop", // existing user
			"bob/phone",    // new user
		} {
			out, err := capture(t, "-config", cfg, "device", "add", device)
			if err == nil || out != "" {
				t.Fatalf("device add %s: out %q, err %v", device, out, err)
			}
			if after := userState(t, cfg); after != before {
				t.Errorf("device add %s changed the store:\nbefore %s\nafter  %s", device, before, after)
			}
		}
	})

	t.Run("invalid route", func(t *testing.T) {
		cfg := testConfig(t)
		mustRun(t, "-config", cfg, "device", "add", "alice/phone")
		before := userState(t, cfg)
		for _, args := range [][]string{{"alice/laptop", "FR"}, {"bob/phone", "NL:beta"}} {
			out, err := capture(t, append([]string{"-config", cfg, "device", "add"}, args...)...)
			if err == nil || !strings.HasPrefix(err.Error(), "route: ") || out != "" {
				t.Fatalf("device add %q: out %q, err %v", args, out, err)
			}
			if after := userState(t, cfg); after != before {
				t.Errorf("device add %q changed the store:\nbefore %s\nafter  %s", args, before, after)
			}
		}
	})

	t.Run("address allocation fails", func(t *testing.T) {
		cfg := testConfig(t)
		data, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg, append(data, "subnet: 10.77.0.0/24\n"...), 0o600); err != nil {
			t.Fatal(err)
		}
		mustRun(t, "-config", cfg, "device", "add", "alice/phone")
		// Fill the rest of the /24: .2-.254 are usable.
		e, err := openEnv(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 252; i++ {
			if _, err := e.store.AddUserPeer("filler", fmt.Sprintf("d%d", i), policy.DefaultRoute(), e.cfg.Subnet); err != nil {
				t.Fatal(err)
			}
		}
		e.store.Close()
		before := userState(t, cfg)
		for _, device := range []string{"alice/laptop", "bob/phone"} {
			out, err := capture(t, "-config", cfg, "device", "add", device)
			if !errors.Is(err, store.ErrSubnetFull) || out != "" {
				t.Fatalf("device add %s: out %q, err %v", device, out, err)
			}
			if after := userState(t, cfg); after != before {
				t.Errorf("device add %s changed the store:\nbefore %s\nafter  %s", device, before, after)
			}
		}
	})
}

// Once the peer is committed, a failed write of its config is reported with
// the way to get the config again.
func TestDeviceAddReportsStdoutFailure(t *testing.T) {
	cfg := testConfig(t)
	r, w, _ := os.Pipe()
	r.Close() // writes to w now fail with EPIPE
	old := os.Stdout
	os.Stdout = w
	err := run([]string{"-config", cfg, "device", "add", "alice/phone"})
	os.Stdout = old
	w.Close()
	if err == nil || !strings.Contains(err.Error(), "`device show alice/phone`") {
		t.Fatalf("err = %v, want the device show hint", err)
	}
	shown, err := capture(t, "-config", cfg, "device", "show", "alice/phone")
	if err != nil {
		t.Fatalf("the peer should have been committed: %v", err)
	}
	if _, err := awgconf.Parse(strings.NewReader(shown)); err != nil {
		t.Errorf("device show is not a config: %v", err)
	}
}
