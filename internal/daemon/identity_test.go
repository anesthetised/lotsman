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
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s|%+v", id.PrivateKey, id.Params))))
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

func TestIdentityLoadsLegacyCapitalizedKeys(t *testing.T) {
	key, err := awgconf.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), fmt.Appendf(nil, legacyIdentity, key), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := awgconf.Params{
		S1: 21, S2: 22, S3: 23, S4: 9,
		H1: awgconf.Range{Lo: 65536, Hi: 69631}, H2: awgconf.Range{Lo: 1114112, Hi: 1118207},
		H3: awgconf.Range{Lo: 2162688, Hi: 2166783}, H4: awgconf.Range{Lo: 3211264, Hi: 3211264},
	}
	if id.PrivateKey != key || id.Params != want {
		t.Errorf("legacy identity = %+v, want key %s params %+v", id.Params, key, want)
	}
}

func TestIdentityRoundTripUsesLowercaseKeys(t *testing.T) {
	want, err := newIdentity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity.json")
	if err := saveIdentity(path, want); err != nil {
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
	for _, k := range []string{"private_key", "s1", "s2", "s3", "s4", "h1", "h2", "h3", "h4"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("saved file lacks key %q: %s", k, data)
		}
	}
	if len(raw) != 9 {
		t.Errorf("saved file has %d keys, want 9: %s", len(raw), data)
	}
}
