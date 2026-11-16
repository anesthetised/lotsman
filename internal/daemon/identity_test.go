package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/anesthetised/lotsman/internal/awgconf"
)

const identityHelperEnv = "LOTSMAN_IDENTITY_HELPER_DIR"

// fingerprint identifies an identity without exposing key material in logs.
func fingerprint(id Identity) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s|%+v|%s", id.PrivateKey, id.Params, id.HeaderProtectionKey))))
}

// checkPublished asserts dir holds exactly the one private identity file, no temp leftovers.
func checkPublished(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "identity.json" {
		t.Errorf("state dir entries = %v, want only identity.json", entries)
	}
	info, err := os.Stat(filepath.Join(dir, "identity.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("identity file: %v, mode %v", err, info)
	}
}

func TestIdentityConcurrentCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	const n = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	got := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, err := LoadOrCreateIdentity(dir)
			errs[i], got[i] = err, fingerprint(id)
		}()
	}
	close(start)
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if got[i] != got[0] {
			t.Fatalf("caller %d saw a different identity than caller 0", i)
		}
	}
	saved, err := LoadOrCreateIdentity(dir)
	if err != nil || fingerprint(saved) != got[0] {
		t.Errorf("saved identity differs from the one returned to callers: %v", err)
	}
	checkPublished(t, dir)
}

func TestIdentityExistingNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
	for range 8 {
		again, err := LoadOrCreateIdentity(dir)
		if err != nil || fingerprint(again) != fingerprint(first) {
			t.Fatalf("existing identity changed: %v", err)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "identity.json")); string(after) != string(before) {
		t.Error("identity file was rewritten")
	}
	checkPublished(t, dir)
}

// TestIdentityHelper is the child side of TestIdentityConcurrentProcesses.
func TestIdentityHelper(t *testing.T) {
	dir := os.Getenv(identityHelperEnv)
	if dir == "" {
		t.Skip("helper for TestIdentityConcurrentProcesses")
	}
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("fingerprint:", fingerprint(id))
}

func TestIdentityConcurrentProcesses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	const n = 8
	var wg sync.WaitGroup
	outs := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestIdentityHelper$")
			cmd.Env = append(os.Environ(), identityHelperEnv+"="+dir)
			out, err := cmd.CombinedOutput()
			errs[i] = err
			for line := range strings.SplitSeq(string(out), "\n") {
				if fp, ok := strings.CutPrefix(line, "fingerprint: "); ok {
					outs[i] = fp
				}
			}
			if err != nil {
				outs[i] = string(out)
			}
		}()
	}
	wg.Wait()
	saved, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("process %d: %v\n%s", i, errs[i], outs[i])
		}
		if outs[i] != fingerprint(saved) {
			t.Fatalf("process %d saw a different identity than the saved one", i)
		}
	}
	checkPublished(t, dir)
}

// legacyIdentity is a file as written before the explicit lowercase tags.
const legacyIdentity = `{
  "private_key": "%s",
  "S1": 21, "S2": 22, "S3": 23, "S4": 9,
  "H1": "65536-69631", "H2": "1114112-1118207",
  "H3": "2162688-2166783", "H4": "3211264"
}`

// awg2Identity is a file as written before AWG 3 support, when S3 started at
// 10 and S4 at 8: no header protection key, paddings too short for one.
const awg2Identity = `{
  "private_key": "%s",
  "s1": 21, "s2": 22, "s3": 10, "s4": 9,
  "h1": "65536-69631", "h2": "1114112-1118207",
  "h3": "2162688-2166783", "h4": "3211264"
}`

// writeIdentityFile writes a hand-made identity file for key and returns its directory.
func writeIdentityFile(t *testing.T, format string, key awgconf.Key) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), fmt.Appendf(nil, format, key), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestIdentityLoadsLegacyCapitalizedKeys(t *testing.T) {
	key, err := awgconf.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	id, err := LoadOrCreateIdentity(writeIdentityFile(t, legacyIdentity, key))
	if err != nil {
		t.Fatal(err)
	}
	// S4 is raised to the header protection minimum by the AWG 3 upgrade.
	want := awgconf.Params{
		S1: 21, S2: 22, S3: 23, S4: 12,
		H1: awgconf.Range{Lo: 65536, Hi: 69631}, H2: awgconf.Range{Lo: 1114112, Hi: 1118207},
		H3: awgconf.Range{Lo: 2162688, Hi: 2166783}, H4: awgconf.Range{Lo: 3211264, Hi: 3211264},
	}
	if id.PrivateKey != key || id.Params != want {
		t.Errorf("legacy identity = %+v, want key %s params %+v", id.Params, key, want)
	}
}

// An identity written before AWG 3 support gains a header protection key and
// S1-S4 of at least 12 on load, keeps its key and headers, and is saved
// once, atomically; level 3 can then use it.
func TestIdentityUpgradesAWG2File(t *testing.T) {
	key, err := awgconf.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := writeIdentityFile(t, awg2Identity, key)
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := awgconf.Params{
		S1: 21, S2: 22, S3: 12, S4: 12,
		H1: awgconf.Range{Lo: 65536, Hi: 69631}, H2: awgconf.Range{Lo: 1114112, Hi: 1118207},
		H3: awgconf.Range{Lo: 2162688, Hi: 2166783}, H4: awgconf.Range{Lo: 3211264, Hi: 3211264},
	}
	if id.PrivateKey != key || id.Params != want || id.HeaderProtectionKey.IsZero() {
		t.Fatalf("upgraded identity = %+v, HeaderProtectionKey zero %v; want params %+v", id.Params, id.HeaderProtectionKey.IsZero(), want)
	}
	checkPublished(t, dir)
	saved, err := loadIdentity(filepath.Join(dir, "identity.json"))
	if err != nil || saved != id {
		t.Fatalf("upgrade not saved: %v", err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
	if again, err := LoadOrCreateIdentity(dir); err != nil || again != id {
		t.Fatalf("second load changed the upgraded identity: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "identity.json")); string(after) != string(before) {
		t.Error("upgraded identity file was rewritten")
	}
	// The runtime refuses a header protection key with short paddings; awgconf
	// mirrors that, so a level-3 client config must parse.
	conf := awgconf.Config{Interface: awgconf.Interface{PrivateKey: key, Params: id.Shared(3)}}
	if _, err := awgconf.Parse(strings.NewReader(conf.String())); err != nil {
		t.Errorf("upgraded identity unusable at level 3: %v", err)
	}
}

// Concurrent loaders of a pre-AWG 3 file all upgrade it to the same identity,
// the one left on disk.
func TestIdentityConcurrentUpgrade(t *testing.T) {
	key, err := awgconf.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := writeIdentityFile(t, awg2Identity, key)
	const n = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	got := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, err := LoadOrCreateIdentity(dir)
			errs[i], got[i] = err, fingerprint(id)
		}()
	}
	close(start)
	wg.Wait()
	saved, err := loadIdentity(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if got[i] != fingerprint(saved) {
			t.Fatalf("caller %d saw a different identity than the saved one", i)
		}
	}
	checkPublished(t, dir)
}

// New identities serve both levels: S1-S4 are at least 12 and the header
// protection key is set, but only level 3 uses it.
func TestNewIdentityLevels(t *testing.T) {
	for range 100 {
		id, err := newIdentity()
		if err != nil {
			t.Fatal(err)
		}
		p := id.Params
		if p.S1 < 15 || p.S1 > 100 || p.S2 < 15 || p.S2 > 100 || p.S3 < 12 || p.S3 > 60 || p.S4 < 12 || p.S4 > 16 {
			t.Fatalf("S1-S4 out of range: %+v", p)
		}
		if id.HeaderProtectionKey.IsZero() || id.HeaderProtectionKey == deriveHeaderProtectionKey(id.PrivateKey) {
			t.Fatalf("header protection key is not random: %s", id.HeaderProtectionKey)
		}
		if l2 := id.Shared(2); l2 != p {
			t.Fatalf("level 2 params = %+v, want the AWG 2 params %+v", l2, p)
		}
		want := p
		want.HeaderProtectionKey, want.RandomTrailers = id.HeaderProtectionKey, true
		if l3 := id.Shared(3); l3 != want {
			t.Fatalf("level 3 params = %+v, want %+v", l3, want)
		}
	}
}

func TestIdentityRoundTripUsesLowercaseKeys(t *testing.T) {
	want, err := newIdentity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity.json")
	if err := saveIdentity(path, want, os.Link); err != nil {
		t.Fatal(err)
	}
	got, err := loadIdentity(path)
	if err != nil || got != want {
		t.Fatalf("round trip: %+v, %v; want %+v", got, err, want)
	}
	data, _ := os.ReadFile(path)
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	keys := []string{"private_key", "s1", "s2", "s3", "s4", "h1", "h2", "h3", "h4", "header_protection_key"}
	for _, k := range keys {
		if _, ok := raw[k]; !ok {
			t.Errorf("saved file lacks key %q: %s", k, data)
		}
	}
	if len(raw) != len(keys) {
		t.Errorf("saved file has %d keys, want %d: %s", len(raw), len(keys), data)
	}
}

func TestIdentityRejectsBadHeaderProtectionKey(t *testing.T) {
	key, err := awgconf.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(awg2Identity, `"s1"`, `"header_protection_key": "c2hvcnQ=", "s1"`, 1)
	if _, err := LoadOrCreateIdentity(writeIdentityFile(t, bad, key)); err == nil || !strings.Contains(err.Error(), "header_protection_key") {
		t.Errorf("short header_protection_key: %v", err)
	}
}
