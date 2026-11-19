package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/config"
)

// A renamed profile takes its peers along; their configs do not change.
func TestProfileRenameCLI(t *testing.T) {
	cfg := testConfig(t) // profiles nl (geo=NL) and auto
	conf, err := capture(t, "-config", cfg, "user", "add", "alice", "-profile", "nl")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := capture(t, "-config", cfg, "profile", "rename", "nl", "Netherlands"); err != nil || out != "" {
		t.Fatalf("profile rename: %v, %q", err, out)
	}
	list, _ := capture(t, "-config", cfg, "profile", "list")
	if got := tableRow(list, "Netherlands"); !reflect.DeepEqual(got, []string{"Netherlands", "1", "geo=NL"}) {
		t.Errorf("profile list row = %q\n%s", got, list)
	}
	if again, err := capture(t, "-config", cfg, "user", "show", "alice", "-profile", "Netherlands"); err != nil || again != conf {
		t.Errorf("config after rename: %v\n%s\nwant\n%s", err, again, conf)
	}

	before := profilesOf(t, cfg)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"profile", "rename", "nl", "fi"}, `profile "nl": not found`},
		{[]string{"profile", "rename", "auto", "NETHERLANDS"}, "the name is taken, ignoring case"},
		{[]string{"profile", "rename", "auto", "Bad Name"}, `profile name "Bad Name" must match`},
		{[]string{"profile", "rename", "auto"}, "want OLD NEW"},
		{[]string{"profile", "rename", "auto", "-x"}, "want OLD NEW"},
	} {
		if _, err := capture(t, append([]string{"-config", cfg}, tc.args...)...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.args, err, tc.want)
		}
		if after := profilesOf(t, cfg); !reflect.DeepEqual(after, before) {
			t.Fatalf("%q changed the profiles:\nbefore %+v\nafter  %+v", tc.args, before, after)
		}
	}
}

// profilesOf reads the test config's profiles back from the store.
func profilesOf(t *testing.T, cfg string) []config.Profile {
	t.Helper()
	e, err := openEnv(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.store.Close()
	profiles, err := e.store.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	return profiles
}

func TestProfileCLI(t *testing.T) {
	cfg := testConfig(t) // profiles nl (geo=NL) and auto

	if _, err := capture(t, "-config", cfg, "user", "add", "alice", "-profile", "nl"); err != nil {
		t.Fatal(err)
	}
	out, err := capture(t, "-config", cfg, "profile", "add", "eu", "geo=NL", "any")
	if err != nil || out != "" {
		t.Fatalf("profile add: %v, %q", err, out)
	}
	list, err := capture(t, "-config", cfg, "profile", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]string{
		{"auto", "0", "(any", "upstream)"},
		{"eu", "0", "geo=NL", "any"},
		{"nl", "1", "geo=NL"},
	} {
		if got := tableRow(list, row[0]); !reflect.DeepEqual(got, row) {
			t.Errorf("profile list row %q = %q\n%s", row[0], got, list)
		}
	}

	// set replaces every tier; list prints them back in the syntax add takes.
	if _, err := capture(t, "-config", cfg, "profile", "set", "eu", "any", "geo=NL"); err != nil {
		t.Fatal(err)
	}
	list, _ = capture(t, "-config", cfg, "profile", "list")
	if got := tableRow(list, "eu"); !reflect.DeepEqual(got, []string{"eu", "0", "any", "geo=NL"}) {
		t.Errorf("after set: %q", got)
	}
	if _, err := capture(t, "-config", cfg, "profile", "set", "eu"); err != nil {
		t.Fatal(err)
	}
	if p := profilesOf(t, cfg)[1]; p.Name != "eu" || len(p.Prefer) != 0 {
		t.Errorf("set without tiers = %+v, want any upstream", p)
	}

	before := profilesOf(t, cfg)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"profile", "add", "nl", "geo=NL"}, "use `profile set`"},
		{[]string{"profile", "set", "fi", "geo=NL"}, "use `profile add`"},
		{[]string{"profile", "add", "fi", "geo=FI"}, `profile "fi": tier 1 geo=FI matches no upstream`},
		{[]string{"profile", "set", "nl", "geo=NL", "geo=FI"}, `profile "nl": tier 2 geo=FI matches no upstream`},
		{[]string{"profile", "add", "Bad Name", "geo=NL"}, `profile name "Bad Name" must match`},
		{[]string{"profile", "add", "NL", "geo=NL"}, "names are unique ignoring case"},
		{[]string{"profile", "add"}, "NAME is required"},
		{[]string{"profile", "add", "-h"}, "NAME is required"},
		{[]string{"profile", "add", "fi", "country=NL"}, `tier "country=NL": want key=value pairs`},
		{[]string{"profile", "add", "fi", "NL"}, `tier "NL": want key=value pairs`},
		{[]string{"profile", "add", "fi", "geo="}, "geo has no value"},
		{[]string{"profile", "add", "fi", "geo=NL,"}, `tier "geo=NL,": want key=value pairs`},
		{[]string{"profile", "add", "fi", "geo=NL,geo=DE"}, "geo is given twice"},
		{[]string{"profile", "set", "nl", "geo=NL", "-prefer"}, `tier "-prefer"`},
		{[]string{"profile", "rm", "nl"}, "is in use"},
		{[]string{"profile", "rm", "fi"}, "not found"},
		{[]string{"profile", "rm"}, "want exactly one NAME"},
		{[]string{"profile", "rm", "eu", "auto"}, "want exactly one NAME"},
		{[]string{"profile", "list", "nl"}, "unexpected argument"},
		{[]string{"profile", "bogus"}, "unknown command"},
	} {
		if _, err := capture(t, append([]string{"-config", cfg}, tc.args...)...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.args, err, tc.want)
		}
		if after := profilesOf(t, cfg); !reflect.DeepEqual(after, before) {
			t.Fatalf("%q changed the profiles:\nbefore %+v\nafter  %+v", tc.args, before, after)
		}
	}

	// A user's profile must exist in the store.
	if _, err := capture(t, "-config", cfg, "user", "add", "bob", "-profile", "fi"); err == nil || !strings.Contains(err.Error(), `profile "fi" does not exist`) {
		t.Errorf("user add with a missing profile: %v", err)
	}
	if strings.Contains(userState(t, cfg), "bob") {
		t.Error("user add with a missing profile created the user")
	}

	// Once its last peer is gone, a profile can go.
	if _, err := capture(t, "-config", cfg, "user", "rm", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(t, "-config", cfg, "profile", "rm", "nl"); err != nil {
		t.Fatal(err)
	}
	if got := profilesOf(t, cfg); len(got) != 2 || got[0].Name != "auto" || got[1].Name != "eu" {
		t.Errorf("after rm: %+v", got)
	}
}

func TestTierSyntaxRoundTrips(t *testing.T) {
	for _, m := range []config.Match{
		{},
		{Geo: "NL"},
		{Provider: "acme"},
		{Tag: "near"},
		{Geo: "NL", Provider: "acme", Tag: "eu"},
		{Geo: "New York", Provider: "a=b"},
	} {
		got, err := parseTier(m.String())
		if err != nil || got != m {
			t.Errorf("%+v -> %q -> %+v, %v", m, m.String(), got, err)
		}
	}
	// Field order on the command line does not matter.
	if m, err := parseTier("tag=eu,geo=NL"); err != nil || m != (config.Match{Geo: "NL", Tag: "eu"}) {
		t.Errorf("tag=eu,geo=NL = %+v, %v", m, err)
	}
}

// An existing deployment moves its profiles with one command: config check
// and serve name it, and it can read them from the config it validates against.
func TestProfileImport(t *testing.T) {
	dir := t.TempDir()
	up := writeProviderConf(t, dir, "nl")
	path := filepath.Join(dir, "lotsman.yaml")
	old := `
endpoint: vpn.example.org:51820
state_dir: ` + filepath.Join(dir, "state") + `
upstreams: [{name: nl, conf: ` + up + `, geo: NL, tags: [eu]}]
profiles:
  - {name: nl, prefer: [{geo: NL}]}
  - {name: eu, prefer: [{tag: eu}, {geo: DE}]}
  - {name: auto}
`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{{"config", "check"}, {"user", "list"}, {"profile", "list"}}
	if runtime.GOOS == "linux" {
		commands = append(commands, []string{"serve"}) // it loads the config before anything else
	}
	for _, args := range commands {
		if _, err := capture(t, append([]string{"-config", path}, args...)...); err == nil || !strings.Contains(err.Error(), "`lotsman -config "+path+" profile import "+path+"`") {
			t.Errorf("%q with a profiles: key: %v", args, err)
		}
	}

	var out string
	var err error
	msg := captureStderr(t, func() { out, err = capture(t, "-config", path, "profile", "import", path) })
	if err != nil || strings.Count(out, "imported") != 3 || !strings.Contains(msg, "delete the profiles: key") {
		t.Fatalf("import: %v\nstdout %s\nstderr %s", err, out, msg)
	}
	if !strings.Contains(msg, `warning: profile "eu": tier 2 geo=DE matches no upstream`) {
		t.Errorf("import did not report the warning:\n%s", msg)
	}
	want := []config.Profile{
		{Name: "auto"},
		{Name: "eu", Prefer: []config.Match{{Tag: "eu"}, {Geo: "DE"}}},
		{Name: "nl", Prefer: []config.Match{{Geo: "NL"}}},
	}
	e, err := openState(mustLoadLegacy(t, path))
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.store.ListProfiles()
	e.store.Close()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("imported = %+v, %v", got, err)
	}

	// Running it again changes nothing.
	msg = captureStderr(t, func() { _, err = capture(t, "-config", path, "profile", "import", path) })
	if err != nil || strings.Count(msg, "already in the store, skipped") != 3 {
		t.Errorf("second import: %v\n%s", err, msg)
	}

	// Without the key the config is valid again.
	if err := os.WriteFile(path, []byte(old[:strings.Index(old, "profiles:")]), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := capture(t, "-config", path, "config", "check"); err != nil || !strings.Contains(out, "profile  eu         prefer tag=eu geo=DE") {
		t.Errorf("config check after import: %v\n%s", err, out)
	}
	if _, err := capture(t, "-config", path, "profile", "import", path); err == nil || !strings.Contains(err.Error(), "has no profiles") {
		t.Errorf("import of a file without profiles: %v", err)
	}
}

// A profile that would fail validation imports nothing at all.
func TestProfileImportIsAllOrNothing(t *testing.T) {
	cfg := testConfig(t)
	old := filepath.Join(t.TempDir(), "old.yaml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, "profiles: [{name: de2, prefer: [{geo: NL}]}, {name: fi, prefer: [{geo: FI}]}]\n"...)
	if err := os.WriteFile(old, data, 0o600); err != nil {
		t.Fatal(err)
	}
	before := profilesOf(t, cfg)
	if _, err := capture(t, "-config", cfg, "profile", "import", old); err == nil || !strings.Contains(err.Error(), `profile "fi": prefer matches no upstream`) {
		t.Fatalf("import = %v", err)
	}
	if after := profilesOf(t, cfg); !reflect.DeepEqual(after, before) {
		t.Errorf("a failed import stored profiles: %+v", after)
	}
}

// Peers whose profile is missing, as after an upgrade without an import,
// are pointed out.
func TestMissingProfileIsWarned(t *testing.T) {
	cfg := testConfig(t)
	if _, err := capture(t, "-config", cfg, "user", "add", "alice", "-profile", "nl"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(cfg), "state", "lotsman.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM profile_tiers; DELETE FROM profiles WHERE name = 'nl'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	const want = `1 peers use profile "nl", which is not in the store`
	if out, err := capture(t, "-config", cfg, "config", "check"); err != nil || !strings.Contains(out, "warning: "+want) {
		t.Errorf("config check: %v\n%s", err, out)
	}
	if msg := captureStderr(t, func() { capture(t, "-config", cfg, "profile", "list") }); !strings.Contains(msg, want) {
		t.Errorf("profile list stderr: %q", msg)
	}
}

func mustLoadLegacy(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, _, err := config.LoadLegacy(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
