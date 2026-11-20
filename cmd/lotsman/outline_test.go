package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/health"
)

const outlineSecret = "never-in-output"

// outlineConfig is testConfig with an Outline upstream next to nl.
func outlineConfig(t *testing.T) string {
	t.Helper()
	path := testConfig(t)
	dir := filepath.Dir(path)
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:" + outlineSecret))
	key := "ss://" + userinfo + "@192.0.2.9:8388/?outline=1&prefix=%16%03%01#My%20key\n"
	if err := os.WriteFile(filepath.Join(dir, "ol.key"), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(mustReadFile(t, path), "upstreams: [{name: nl, conf: "+filepath.Join(dir, "nl.conf")+", geo: NL, provider: acme}]",
		"upstreams: [{name: nl, conf: "+filepath.Join(dir, "nl.conf")+", geo: NL, provider: acme}, {name: ol, kind: outline, conf: "+filepath.Join(dir, "ol.key")+", geo: FI, provider: acme}]", 1)
	if !strings.Contains(cfg, "kind: outline") {
		t.Fatal("test setup: could not add the outline upstream")
	}
	os.WriteFile(path, []byte(cfg), 0o600)
	return path
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestConfigCheckOutline(t *testing.T) {
	path := outlineConfig(t)
	out, err := capture(t, "-config", path, "config", "check")
	if err != nil {
		t.Fatalf("config check: %v\n%s", err, out)
	}
	for _, want := range []string{
		"lm-up-nl  kind awg  geo NL  provider acme  endpoint 192.0.2.1:51820  address 10.8.0.2/32  mtu 1400  s4 8",
		"lm-up-ol  kind outline  geo FI  provider acme  endpoint 192.0.2.9:8388  address 192.0.0.8/32  mtu 1500  shadowsocks chacha20-ietf-poly1305, 3-byte prefix  max_flows 4096",
		"health   probe 1.1.1.1:443  dns 1.1.1.1:53",
		"config OK",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config check output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, outlineSecret) {
		t.Errorf("config check prints the password:\n%s", out)
	}

	// The proxy adds nothing to the client MTU: nl still sets it.
	out, err = capture(t, "-config", path, "device", "add", "alice/phone")
	if err != nil {
		t.Fatal(err)
	}
	client, err := awgconf.Parse(strings.NewReader(out))
	if err != nil || client.Interface.MTU != 1392 {
		t.Errorf("client MTU = %d, %v", client.Interface.MTU, err)
	}
}

func TestUpstreamStatusShowsKind(t *testing.T) {
	path := outlineConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.StateDir, 0o700)
	handshake := time.Now().Add(-time.Minute)
	s := statusFile{UpdatedAt: time.Now(), ClientMTU: 1392, Upstreams: []daemon.UpstreamStatus{
		{Name: "nl", Kind: "awg", Interface: "lm-up-nl", State: health.Up, LastHandshake: handshake, Routed: 1, Active: 1},
		{Name: "ol", Kind: "outline", Interface: "lm-up-ol", State: health.Up, Routed: 2},
	}}
	data, _ := json.Marshal(s)
	os.WriteFile(statusPath(cfg.StateDir), data, 0o600)
	out, err := capture(t, "-config", path, "upstream", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "UPSTREAM  KIND     INTERFACE") {
		t.Errorf("no KIND column:\n%s", out)
	}
	if f := tableFields(out, "ol"); len(f) != 10 || f[1] != "outline" || f[5] != "-" || f[6] != "2" {
		t.Errorf("ol row = %q:\n%s", f, out)
	}
	if f := tableFields(out, "nl"); len(f) != 11 || f[1] != "awg" || f[6] != "ago" {
		t.Errorf("nl row = %q:\n%s", f, out)
	}
}

// A dynamic key that cannot be fetched is a warning: config check runs
// before the daemon starts, which keeps that upstream down and retries.
func TestConfigCheckDynamicKeyUnreachable(t *testing.T) {
	path := outlineConfig(t)
	// Port 1 on loopback refuses at once.
	os.WriteFile(filepath.Join(filepath.Dir(path), "ol.key"), []byte("ssconf://127.0.0.1:1/"+outlineSecret+"#name\n"), 0o600)
	out, err := capture(t, "-config", path, "config", "check")
	if err != nil {
		t.Fatalf("config check: %v\n%s", err, out)
	}
	for _, want := range []string{
		"lm-up-ol  kind outline  dynamic key not fetched",
		"warning: upstream ol: fetch dynamic key: ",
		"config OK",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config check output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, outlineSecret) {
		t.Errorf("config check prints the URL:\n%s", out)
	}
}

// tableFields returns the fields of the first line whose first field is key.
func tableFields(table, key string) []string {
	for line := range strings.Lines(table) {
		if f := strings.Fields(line); len(f) > 0 && f[0] == key {
			return f
		}
	}
	return nil
}
