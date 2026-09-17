package main

import (
	"bytes"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/awgconf"
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
