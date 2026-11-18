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
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
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
endpoint: vpn.example.org:51820
state_dir: `+filepath.Join(dir, "state")+`
upstreams: [{name: nl, conf: `+up+`, geo: NL}]
profiles: [{name: nl, prefer: [{geo: NL}]}, {name: auto}]
`), 0o600)
	return path
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
	if err != nil || !strings.Contains(out, "config OK") || !strings.Contains(out, "lm-up-nl") {
		t.Fatalf("config check: %v\n%s", err, out)
	}

	out, err = capture(t, "-config", cfg, "user", "add", "alice", "-profile", "nl")
	if err != nil {
		t.Fatal(err)
	}
	client, err := awgconf.Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("user add output is not a config: %v\n%s", err, out)
	}
	if client.Interface.MTU != 1392 || client.Interface.Addresses[0].String() != "10.77.0.2/32" || client.Peers[0].Endpoint != "vpn.example.org:51820" {
		t.Errorf("client config = %+v", client)
	}

	if _, err := capture(t, "-config", cfg, "user", "add", "alice", "-profile", "nl"); err == nil {
		t.Error("duplicate profile accepted")
	}
	if _, err := capture(t, "-config", cfg, "user", "add", "alice", "-profile", "nope"); err == nil {
		t.Error("unknown profile accepted")
	}
	if _, err := capture(t, "-config", cfg, "user", "add", "alice"); err == nil {
		t.Error("missing -profile accepted")
	}

	again, err := capture(t, "-config", cfg, "user", "show", "alice", "-profile", "nl")
	if err != nil || again != out {
		t.Errorf("user show differs from user add: %v\n%s", err, again)
	}

	capture(t, "-config", cfg, "user", "add", "alice", "-profile", "auto")
	list, err := capture(t, "-config", cfg, "user", "list")
	if err != nil || strings.Count(list, "alice") != 2 || !strings.Contains(list, "10.77.0.3") || !strings.Contains(list, "default") {
		t.Errorf("user list: %v\n%s", err, list)
	}

	// A second device gets its own key even for the same profile.
	phone, err := capture(t, "-config", cfg, "user", "add", "alice", "-device", "phone", "-profile", "nl")
	if err != nil {
		t.Fatal(err)
	}
	if phone == out {
		t.Error("phone config identical to the default device's config")
	}
	if _, err := capture(t, "-config", cfg, "user", "add", "alice", "-device", "phone", "-profile", "nl"); err == nil {
		t.Error("duplicate device+profile accepted")
	}
	capture(t, "-config", cfg, "user", "add", "alice", "-device", "phone", "-profile", "auto")

	if _, err := capture(t, "-config", cfg, "user", "rm", "alice", "-profile", "auto"); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(t, "-config", cfg, "user", "rm", "alice", "-device", "phone"); err != nil {
		t.Fatal(err)
	}
	list, _ = capture(t, "-config", cfg, "user", "list")
	if strings.Count(list, "alice") != 1 || strings.Contains(list, "phone") {
		t.Errorf("after removing profile and device:\n%s", list)
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
	withQR, err := capture(t, "-config", cfg, "user", "add", "bob", "-profile", "nl", "-qr")
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
	for _, a := range [][]string{
		{"alice", "-profile", "nl"},
		{"alice", "-profile", "auto"},
		{"alice", "-device", "phone", "-profile", "nl"},
		{"alice", "-device", "phone", "-profile", "auto"},
	} {
		if _, err := capture(t, append([]string{"-config", cfg, "user", "add"}, a...)...); err != nil {
			t.Fatal(err)
		}
	}
	peers := func() []store.Peer {
		e, err := openEnv(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer e.store.Close()
		ps, err := e.store.ListPeers()
		if err != nil {
			t.Fatal(err)
		}
		return ps
	}
	before := peers()
	if len(before) != 4 {
		t.Fatalf("setup: %d peers, want 4", len(before))
	}

	for _, args := range [][]string{
		{"user", "rm", "alice", "phone", "-profile", "nl"},
		{"user", "rm", "alice", "phone"},
		{"user", "rm", "alice", "-device", "phone", "extra"},
		{"user", "add", "bob", "phone", "-profile", "nl"},
		{"user", "show", "alice", "phone", "-profile", "nl"},
	} {
		_, err := capture(t, append([]string{"-config", cfg}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
			t.Errorf("%v: err = %v, want unexpected argument", args, err)
		}
		if after := peers(); !reflect.DeepEqual(after, before) {
			t.Errorf("%v changed stored peers:\nbefore %+v\nafter  %+v", args, before, after)
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
	before, err := capture(t, "-config", path, "user", "add", "alice", "-profile", "nl")
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
	for _, args := range [][]string{{"config", "check"}, {"user", "add", "bob", "-profile", "nl"}} {
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
	after, err := capture(t, "-config", path, "user", "show", "alice", "-profile", "nl")
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
	before, err := capture(t, "-config", path, "user", "add", "alice", "-profile", "nl")
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
	for _, endpoint := range []string{":51820", "vpn.example.org:banana", "vpn.example.org:0", "vpn.example.org:65536"} {
		t.Run(endpoint, func(t *testing.T) {
			invalid := strings.Replace(string(original), "endpoint: vpn.example.org:51820", "endpoint: '"+endpoint+"'", 1)
			if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"config", "check"}, {"user", "add", "bob", "-profile", "nl"}, {"user", "show", "alice", "-profile", "nl"}} {
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
	after, err := capture(t, "-config", path, "user", "show", "alice", "-profile", "nl")
	if err != nil || after != before {
		t.Errorf("valid endpoint no longer reproduces stored peer: %v", err)
	}
}

// Client configs depend on provider MTU and S4 only, so user commands must work
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
	added, err := capture(t, "-config", path, "user", "add", "alice", "-profile", "nl")
	if err != nil {
		t.Fatal(err)
	}
	if client, err := awgconf.Parse(strings.NewReader(added)); err != nil || client.Interface.MTU != 1392 {
		t.Errorf("user add output is not a config with MTU 1392: %v\n%s", err, added)
	}
	shown, err := capture(t, "-config", path, "user", "show", "alice", "-profile", "nl")
	if err != nil || shown != added {
		t.Errorf("user show: %v\n%s", err, shown)
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

// An empty -profile, typically an unset shell variable, must not widen
// `user rm` to the whole device or the whole user.
func TestUserRemoveRejectsEmptyProfile(t *testing.T) {
	cfg := testConfig(t)
	for _, add := range [][]string{
		{"alice", "-profile", "nl"},
		{"alice", "-profile", "auto"},
		{"alice", "-profile", "nl", "-device", "phone"},
		{"alice", "-profile", "auto", "-device", "phone"},
	} {
		if _, err := capture(t, append([]string{"-config", cfg, "user", "add"}, add...)...); err != nil {
			t.Fatal(err)
		}
	}
	before := userState(t, cfg)
	for _, args := range [][]string{
		{"alice", "-profile", ""},
		{"alice", "-profile="},
		{"alice", "-profile", "", "-device", "phone"},
		{"alice", "-device", "phone", "-profile="},
		{"alice", "-device", "default", "-profile", ""},
	} {
		_, err := capture(t, append([]string{"-config", cfg, "user", "rm"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "-profile must not be empty") {
			t.Errorf("user rm %q: err = %v", args, err)
		}
		if after := userState(t, cfg); after != before {
			t.Fatalf("user rm %q changed the store:\nbefore %s\nafter  %s", args, before, after)
		}
	}
	for _, cmd := range []string{"add", "show"} {
		if _, err := capture(t, "-config", cfg, "user", cmd, "alice", "-profile", ""); err == nil {
			t.Errorf("user %s accepted an empty profile", cmd)
		}
	}
	// The documented forms still work when -profile is omitted.
	if _, err := capture(t, "-config", cfg, "user", "rm", "alice", "-device", "phone"); err != nil {
		t.Fatal(err)
	}
	if list, _ := capture(t, "-config", cfg, "user", "list"); strings.Contains(list, "phone") || strings.Count(list, "alice") != 2 {
		t.Errorf("whole-device removal:\n%s", list)
	}
	if _, err := capture(t, "-config", cfg, "user", "rm", "alice"); err != nil {
		t.Fatal(err)
	}
	if list, _ := capture(t, "-config", cfg, "user", "list"); strings.Contains(list, "alice") {
		t.Errorf("whole-user removal:\n%s", list)
	}
}

// A rejected `user add` must leave users and peers exactly as they were.
func TestUserAddFailureLeavesNothingBehind(t *testing.T) {
	t.Run("unreadable provider file", func(t *testing.T) {
		cfg := testConfig(t)
		if _, err := capture(t, "-config", cfg, "user", "add", "alice", "-profile", "nl"); err != nil {
			t.Fatal(err)
		}
		before := userState(t, cfg)
		if err := os.Remove(filepath.Join(filepath.Dir(cfg), "nl.conf")); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"alice", "-profile", "auto"}, // existing user
			{"bob", "-profile", "nl"},     // new user
		} {
			out, err := capture(t, append([]string{"-config", cfg, "user", "add"}, args...)...)
			if err == nil || out != "" {
				t.Fatalf("user add %q: out %q, err %v", args, out, err)
			}
			if after := userState(t, cfg); after != before {
				t.Errorf("user add %q changed the store:\nbefore %s\nafter  %s", args, before, after)
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
		if _, err := capture(t, "-config", cfg, "user", "add", "alice", "-profile", "nl"); err != nil {
			t.Fatal(err)
		}
		// Fill the rest of the /24: .2-.254 are usable.
		e, err := openEnv(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 252; i++ {
			if _, err := e.store.AddUserPeer("filler", fmt.Sprintf("d%d", i), "nl", e.cfg.Subnet); err != nil {
				t.Fatal(err)
			}
		}
		e.store.Close()
		before := userState(t, cfg)
		for _, args := range [][]string{
			{"alice", "-profile", "auto"},
			{"bob", "-profile", "nl"},
		} {
			out, err := capture(t, append([]string{"-config", cfg, "user", "add"}, args...)...)
			if !errors.Is(err, store.ErrSubnetFull) || out != "" {
				t.Fatalf("user add %q: out %q, err %v", args, out, err)
			}
			if after := userState(t, cfg); after != before {
				t.Errorf("user add %q changed the store:\nbefore %s\nafter  %s", args, before, after)
			}
		}
	})
}

// Once the peer is committed, a failed write of its config is reported with
// the way to get the config again.
func TestUserAddReportsStdoutFailure(t *testing.T) {
	cfg := testConfig(t)
	r, w, _ := os.Pipe()
	r.Close() // writes to w now fail with EPIPE
	old := os.Stdout
	os.Stdout = w
	err := run([]string{"-config", cfg, "user", "add", "alice", "-profile", "nl", "-device", "phone"})
	os.Stdout = old
	w.Close()
	if err == nil || !strings.Contains(err.Error(), "`user show alice -profile nl -device phone`") {
		t.Fatalf("err = %v, want the user show hint", err)
	}
	shown, err := capture(t, "-config", cfg, "user", "show", "alice", "-profile", "nl", "-device", "phone")
	if err != nil {
		t.Fatalf("the peer should have been committed: %v", err)
	}
	if _, err := awgconf.Parse(strings.NewReader(shown)); err != nil {
		t.Errorf("user show is not a config: %v", err)
	}
}
