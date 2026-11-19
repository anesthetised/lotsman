package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/policy"
	"github.com/anesthetised/lotsman/internal/store"
)

// routesConfig is testConfig with upstreams in two countries and, in NL, of
// two providers: nl (NL, acme), nb (NL, beta) and de (DE, acme).
func routesConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var ups []string
	for _, u := range []struct{ name, geo, provider string }{{"nl", "NL", "acme"}, {"nb", "NL", "beta"}, {"de", "DE", "acme"}} {
		ups = append(ups, "{name: "+u.name+", conf: "+writeProviderConf(t, dir, u.name)+", geo: "+u.geo+", provider: "+u.provider+"}")
	}
	path := filepath.Join(dir, "lotsman.yaml")
	if err := os.WriteFile(path, []byte(`
endpoint: vpn.example.org:51820
state_dir: `+filepath.Join(dir, "state")+`
upstreams: [`+strings.Join(ups, ", ")+`]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// storedPeer reads one device back from the store.
func storedPeer(t *testing.T, cfg, user, device string) store.Peer {
	t.Helper()
	e, err := openEnv(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.store.Close()
	p, err := e.store.Peer(user, device)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// storedUser reads one user back from the store.
func storedUser(t *testing.T, cfg, name string) store.User {
	t.Helper()
	e, err := openEnv(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.store.Close()
	u, err := e.store.User(name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The countries after the flags are the route, in order; `device route`
// replaces it and leaves the config as it was.
func TestDeviceRouteCLI(t *testing.T) {
	cfg := routesConfig(t)
	conf := mustRun(t, "-config", cfg, "device", "add", "alice/phone", "-fastest", "-fallback", "nl", "DE:acme")
	want := policy.Route{Fastest: true, Countries: []policy.Country{{Geo: "NL"}, {Geo: "DE", Providers: []string{"acme"}}}}
	if got := storedPeer(t, cfg, "alice", "phone").Route; !reflect.DeepEqual(got, want) {
		t.Errorf("route after device add = %+v, want %+v", got, want)
	}
	if row := tableRow(mustRun(t, "-config", cfg, "user", "list"), "alice"); !strings.Contains(strings.Join(row, " "), "fastest NL DE:acme > any") {
		t.Errorf("user list row = %q", row)
	}

	for _, tc := range []struct {
		args []string
		want policy.Route
	}{
		{[]string{"NL:beta,acme", "DE"}, policy.Route{Strict: true, Countries: []policy.Country{{Geo: "NL", Providers: []string{"beta", "acme"}}, {Geo: "DE"}}}},
		{[]string{"-fallback", "DE"}, policy.Route{Countries: []policy.Country{{Geo: "DE"}}}},
		{nil, policy.DefaultRoute()},
		{[]string{"-fallback"}, policy.DefaultRoute()}, // without countries the flags change nothing
	} {
		if out, err := capture(t, append([]string{"-config", cfg, "device", "route", "alice/phone"}, tc.args...)...); err != nil || out != "" {
			t.Fatalf("device route %q: %v, %q", tc.args, err, out)
		}
		if got := storedPeer(t, cfg, "alice", "phone").Route; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("device route %q stored %+v, want %+v", tc.args, got, tc.want)
		}
	}
	if again := mustRun(t, "-config", cfg, "device", "show", "alice/phone"); again != conf {
		t.Errorf("the config changed with the route:\n%s\nwant\n%s", again, conf)
	}

	before := userState(t, cfg)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"alice/phone", "FR"}, `route: no upstream in "FR"`},
		{[]string{"alice/phone", "DE:beta"}, `route: no upstream of provider "beta" in "DE"`},
		{[]string{"alice/phone", "NL", "nl"}, `route: "NL" is listed twice`},
		{[]string{"alice/phone", "NL:acme,acme"}, `route: provider "acme" is listed twice in "NL"`},
		{[]string{"alice/phone", "DE:"}, `"DE:": want COUNTRY or COUNTRY:PROVIDER,PROVIDER`},
		{[]string{"alice/phone", "NL:acme,"}, `want COUNTRY or COUNTRY:PROVIDER,PROVIDER`},
		{[]string{"alice/phone", ":acme"}, `want COUNTRY or COUNTRY:PROVIDER,PROVIDER`},
		{[]string{"alice/tablet", "NL"}, "alice/tablet: not found"},
		{[]string{"bob/phone", "NL"}, "bob/phone: not found"},
	} {
		_, err := capture(t, append([]string{"-config", cfg, "device", "route"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("device route %q: err = %v, want %q", tc.args, err, tc.want)
		}
		if after := userState(t, cfg); after != before {
			t.Fatalf("device route %q changed the store:\nbefore %s\nafter  %s", tc.args, before, after)
		}
	}
}

// `user rights` shows the rights, or changes the ones given, and routes
// written afterwards must stay within the exits.
func TestUserRightsCLI(t *testing.T) {
	cfg := routesConfig(t)
	if out, err := capture(t, "-config", cfg, "user", "add", "alice"); err != nil || out != "" {
		t.Fatalf("user add: %v, %q", err, out)
	}
	if _, err := capture(t, "-config", cfg, "user", "add", "alice"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("user add twice: %v", err)
	}
	const defaults = "edit-routes     true\nshow-providers  true\nexits           all\n"
	if out := mustRun(t, "-config", cfg, "user", "rights", "alice"); out != defaults {
		t.Errorf("user rights =\n%s\nwant\n%s", out, defaults)
	}

	out := mustRun(t, "-config", cfg, "user", "rights", "alice", "-edit-routes=false", "-exits", "nl,de")
	if want := "edit-routes     false\nshow-providers  true\nexits           nl,de\n"; out != want {
		t.Errorf("user rights after a change =\n%s\nwant\n%s", out, want)
	}
	want := store.Rights{ShowProviders: true, Exits: []string{"nl", "de"}}
	if got := storedUser(t, cfg, "alice").Rights; !reflect.DeepEqual(got, want) {
		t.Errorf("stored rights = %+v, want %+v", got, want)
	}
	// Only the flags given change.
	mustRun(t, "-config", cfg, "user", "rights", "alice", "-show-providers=false")
	want.ShowProviders = false
	if got := storedUser(t, cfg, "alice").Rights; !reflect.DeepEqual(got, want) {
		t.Errorf("stored rights = %+v, want %+v", got, want)
	}

	// New routes must use the exits: nb is not one.
	if _, err := capture(t, "-config", cfg, "device", "add", "alice/phone", "NL:beta"); err == nil ||
		!strings.Contains(err.Error(), `route: no allowed exit of provider "beta" in "NL"`) {
		t.Errorf("device add outside the exits: %v", err)
	}
	mustRun(t, "-config", cfg, "device", "add", "alice/phone", "NL")

	before := userState(t, cfg)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"alice", "-exits", "fr"}, `exits: no upstream "fr"`},
		{[]string{"alice", "-exits", "nl,nl"}, `exits: "nl" is listed twice`},
		{[]string{"alice", "-exits", ""}, "want upstream names joined by commas, or all"},
		{[]string{"alice", "-exits", "nl,"}, "want upstream names joined by commas, or all"},
		{[]string{"alice", "-edit-routes=maybe"}, "invalid boolean value"},
		{[]string{"bob", "-exits", "all"}, "bob: not found"},
		{[]string{"-exits", "all"}, "NAME is required"},
	} {
		_, err := capture(t, append([]string{"-config", cfg, "user", "rights"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("user rights %q: err = %v, want %q", tc.args, err, tc.want)
		}
		if after := userState(t, cfg); after != before {
			t.Fatalf("user rights %q changed the store:\nbefore %s\nafter  %s", tc.args, before, after)
		}
	}

	mustRun(t, "-config", cfg, "user", "rights", "alice", "-exits", "all", "-edit-routes")
	if got := storedUser(t, cfg, "alice").Rights; !reflect.DeepEqual(got, store.Rights{EditRoutes: true}) {
		t.Errorf("rights after -exits all = %+v", got)
	}
	mustRun(t, "-config", cfg, "device", "route", "alice/phone", "NL:beta")
}
