package policy

import (
	"slices"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
)

func up(name, geo string, healthy bool, latency time.Duration) Upstream {
	return Upstream{
		Upstream: config.Upstream{Name: name, Geo: geo, Provider: "acme"},
		Healthy:  healthy,
		Latency:  latency,
	}
}

func degraded(u Upstream) Upstream {
	u.Degraded = true
	return u
}

func TestSelect(t *testing.T) {
	nl := []Tier{{{Geo: "NL"}}}
	eu := []Tier{{{Geo: "NL"}}, {{Geo: "DE"}}}
	anyTier := []Tier{{{}}}

	cases := []struct {
		name      string
		tiers     []Tier
		upstreams []Upstream
		current   string
		want      string
		wantOK    bool
	}{
		{"nothing healthy fails closed", nl,
			[]Upstream{up("nl1", "NL", false, 0)}, "", "", false},
		{"no upstream matches fails closed", nl,
			[]Upstream{up("de1", "DE", true, 0)}, "", "", false},
		{"no tiers fails closed", nil,
			[]Upstream{up("de1", "DE", true, 0)}, "", "", false},
		{"picks matching healthy", nl,
			[]Upstream{up("de1", "DE", true, 0), up("nl1", "NL", true, 0)}, "", "nl1", true},
		{"lowest latency wins in a tier", nl,
			[]Upstream{up("nl1", "NL", true, 50*time.Millisecond), up("nl2", "NL", true, 20*time.Millisecond)}, "", "nl2", true},
		{"equal latency breaks ties by name", nl,
			[]Upstream{up("nl2", "NL", true, 5), up("nl1", "NL", true, 5)}, "", "nl1", true},
		{"sticky to current despite faster sibling", nl,
			[]Upstream{up("nl1", "NL", true, 50), up("nl2", "NL", true, 20)}, "nl1", "nl1", true},
		{"leaves current when it goes down", nl,
			[]Upstream{up("nl1", "NL", false, 50), up("nl2", "NL", true, 20)}, "nl1", "nl2", true},
		{"falls back to second tier", eu,
			[]Upstream{up("nl1", "NL", false, 0), up("de1", "DE", true, 0)}, "", "de1", true},
		{"moves back when preferred tier recovers", eu,
			[]Upstream{up("nl1", "NL", true, 0), up("de1", "DE", true, 0)}, "de1", "nl1", true},
		{"any matches everything", anyTier,
			[]Upstream{up("de1", "DE", true, 30), up("nl1", "NL", true, 10)}, "", "nl1", true},
		{"any is sticky too", anyTier,
			[]Upstream{up("de1", "DE", true, 30), up("nl1", "NL", true, 10)}, "de1", "de1", true},
		{"current outside the tiers is not kept", nl,
			[]Upstream{up("de1", "DE", true, 0), up("nl1", "NL", true, 0)}, "de1", "nl1", true},
		{"leaves current when it is degraded", nl,
			[]Upstream{degraded(up("nl1", "NL", true, 10)), up("nl2", "NL", true, 50)}, "nl1", "nl2", true},
		{"a worse tier beats a degraded better one", eu,
			[]Upstream{degraded(up("nl1", "NL", true, 0)), up("de1", "DE", true, 0)}, "nl1", "de1", true},
		{"degraded is the last resort", nl,
			[]Upstream{degraded(up("nl1", "NL", true, 0)), up("de1", "DE", true, 0)}, "", "nl1", true},
		{"last resort keeps tiers", eu,
			[]Upstream{degraded(up("de1", "DE", true, 0)), degraded(up("nl1", "NL", true, 50))}, "", "nl1", true},
		{"last resort is sticky", nl,
			[]Upstream{degraded(up("nl1", "NL", true, 50)), degraded(up("nl2", "NL", true, 20))}, "nl1", "nl1", true},
		{"last resort picks lowest latency", nl,
			[]Upstream{degraded(up("nl1", "NL", true, 50)), degraded(up("nl2", "NL", true, 20))}, "", "nl2", true},
		{"degraded and down fails closed", nl,
			[]Upstream{degraded(up("nl1", "NL", false, 0))}, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Select(c.tiers, c.upstreams, c.current)
			if got != c.want || ok != c.wantOK {
				t.Errorf("Select = (%q, %v), want (%q, %v)", got, ok, c.want, c.wantOK)
			}
		})
	}
}

// labeled is an upstream with a provider of its own.
func labeled(name, geo, provider string, healthy bool, latency time.Duration) Upstream {
	u := up(name, geo, healthy, latency)
	u.Provider = provider
	return u
}

// Routes compile to tiers that Select then applies: the order of countries,
// latency across them, provider subsets and the fallback to any exit.
func TestSelectRoute(t *testing.T) {
	nl, de := Country{Geo: "NL"}, Country{Geo: "DE"}
	ups := func(nlUp, deUp, frUp bool) []Upstream {
		return []Upstream{
			labeled("nl-ac", "NL", "acme", nlUp, 40*time.Millisecond),
			labeled("nl-be", "NL", "beta", nlUp, 30*time.Millisecond),
			labeled("de-ac", "DE", "acme", deUp, 20*time.Millisecond),
			labeled("fr-ac", "FR", "acme", frUp, 5*time.Millisecond),
		}
	}
	for _, tc := range []struct {
		name  string
		route Route
		ups   []Upstream
		want  string
		ok    bool
	}{
		{"ordered: the first country wins over a faster one", Route{Strict: true, Countries: []Country{nl, de}}, ups(true, true, true), "nl-be", true},
		{"ordered: the next country when the first is down", Route{Strict: true, Countries: []Country{nl, de}}, ups(false, true, true), "de-ac", true},
		{"fastest: the lowest latency of every country", Route{Fastest: true, Strict: true, Countries: []Country{nl, de}}, ups(true, true, true), "de-ac", true},
		{"a provider subset", Route{Strict: true, Countries: []Country{{Geo: "NL", Providers: []string{"acme"}}}}, ups(true, true, true), "nl-ac", true},
		{"providers of one country are one tier", Route{Strict: true, Countries: []Country{{Geo: "NL", Providers: []string{"acme", "beta"}}}}, ups(true, true, true), "nl-be", true},
		{"strict: blocked when no country has an exit", Route{Strict: true, Countries: []Country{nl, de}}, ups(false, false, true), "", false},
		{"fallback: any exit when no country has one", Route{Countries: []Country{nl, de}}, ups(false, false, true), "fr-ac", true},
		{"fallback comes after every country", Route{Countries: []Country{nl, de}}, ups(false, true, true), "de-ac", true},
		{"no countries: the fastest of any exit", DefaultRoute(), ups(true, true, true), "fr-ac", true},
		{"no countries, ordered: still any exit", Route{}, ups(true, true, true), "fr-ac", true},
		{"no countries, nothing up: blocked", DefaultRoute(), ups(false, false, false), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Select(tc.route.Tiers(), tc.ups, "")
			if got != tc.want || ok != tc.ok {
				t.Errorf("Select(%v) = %q, %v; want %q, %v", tc.route, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// The caller narrows the upstreams to the user's exits before Select, so
// even a fallback never leaves them.
func TestSelectWithinExits(t *testing.T) {
	ups := []Upstream{up("nl1", "NL", false, 0), up("de1", "DE", true, 10), up("fr1", "FR", true, 5)}
	fallback := Route{Countries: []Country{{Geo: "NL"}}}
	for _, tc := range []struct {
		name  string
		exits []string
		want  string
		ok    bool
	}{
		{"no exits: every upstream", nil, "fr1", true},
		{"falls back within the exits only", []string{"nl1", "de1"}, "de1", true},
		{"only a down exit: blocked", []string{"nl1"}, "", false},
		{"a stale name allows nothing", []string{"gone"}, "", false},
		{"a stale name next to a live one is ignored", []string{"gone", "fr1"}, "fr1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Select(fallback.Tiers(), Exits(ups, tc.exits), "")
			if got != tc.want || ok != tc.ok {
				t.Errorf("Select = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestValidateExits(t *testing.T) {
	upstreams := []config.Upstream{{Name: "nl1"}, {Name: "de1"}}
	for _, tc := range []struct {
		exits   []string
		wantErr string
	}{
		{nil, ""},
		{[]string{"de1", "nl1"}, ""},
		{[]string{"fr1"}, `exits: no upstream "fr1"`},
		{[]string{"nl1", "nl1"}, `exits: "nl1" is listed twice`},
		{[]string{""}, `exits: no upstream ""`},
	} {
		if got := errString(ValidateExits(tc.exits, upstreams)); got != tc.wantErr {
			t.Errorf("ValidateExits(%q) = %q, want %q", tc.exits, got, tc.wantErr)
		}
	}
}

func TestRouteTiers(t *testing.T) {
	nl, de := Country{Geo: "NL"}, Country{Geo: "DE", Providers: []string{"acme", "beta"}}
	deTier := Tier{{Geo: "DE", Provider: "acme"}, {Geo: "DE", Provider: "beta"}}
	for _, tc := range []struct {
		route Route
		want  []Tier
	}{
		{DefaultRoute(), []Tier{{{}}}},
		{Route{}, []Tier{{{}}}},
		{Route{Strict: true, Countries: []Country{nl, de}}, []Tier{{{Geo: "NL"}}, deTier}},
		{Route{Countries: []Country{nl, de}}, []Tier{{{Geo: "NL"}}, deTier, {{}}}},
		{Route{Fastest: true, Strict: true, Countries: []Country{nl, de}}, []Tier{append(Tier{{Geo: "NL"}}, deTier...)}},
		{Route{Fastest: true, Countries: []Country{nl}}, []Tier{{{Geo: "NL"}}, {{}}}},
	} {
		got := tc.route.Tiers()
		if !slices.EqualFunc(got, tc.want, func(a, b Tier) bool { return slices.Equal(a, b) }) {
			t.Errorf("%v: Tiers = %v, want %v", tc.route, got, tc.want)
		}
	}
}

func TestRouteValidate(t *testing.T) {
	upstreams := []config.Upstream{
		{Name: "nl-ac", Geo: "NL", Provider: "acme"},
		{Name: "nl-be", Geo: "NL", Provider: "beta"},
		{Name: "de-ac", Geo: "DE", Provider: "acme"},
	}
	for _, tc := range []struct {
		name    string
		route   Route
		exits   []string
		wantErr string
	}{
		{"no countries", DefaultRoute(), nil, ""},
		{"countries and providers", Route{Countries: []Country{{Geo: "NL", Providers: []string{"beta", "acme"}}, {Geo: "DE"}}}, nil, ""},
		{"unknown country", Route{Countries: []Country{{Geo: "FR"}}}, nil, `route: no upstream in "FR"`},
		{"provider of another country", Route{Countries: []Country{{Geo: "DE", Providers: []string{"beta"}}}}, nil, `route: no upstream of provider "beta" in "DE"`},
		{"duplicate country", Route{Countries: []Country{{Geo: "NL"}, {Geo: "DE"}, {Geo: "NL", Providers: []string{"acme"}}}}, nil, `route: "NL" is listed twice`},
		{"duplicate provider", Route{Countries: []Country{{Geo: "NL", Providers: []string{"acme", "acme"}}}}, nil, `route: provider "acme" is listed twice in "NL"`},
		{"an empty country", Route{Countries: []Country{{}}}, nil, `route: no upstream in ""`},
		{"an empty provider", Route{Countries: []Country{{Geo: "NL", Providers: []string{""}}}}, nil, `route: no upstream of provider "" in "NL"`},
		{"a country within the exits", Route{Countries: []Country{{Geo: "NL"}}}, []string{"nl-be"}, ""},
		{"a country outside the exits", Route{Countries: []Country{{Geo: "DE"}}}, []string{"nl-be"}, `route: no allowed exit in "DE"`},
		{"a provider outside the exits", Route{Countries: []Country{{Geo: "NL", Providers: []string{"acme"}}}}, []string{"nl-be"}, `route: no allowed exit of provider "acme" in "NL"`},
		{"only stale exits", Route{Countries: []Country{{Geo: "NL"}}}, []string{"gone"}, `route: no allowed exit in "NL"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.route.Validate(upstreams, tc.exits)
			if got := errString(err); got != tc.wantErr {
				t.Errorf("Validate = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestRouteString(t *testing.T) {
	nl, de := Country{Geo: "NL"}, Country{Geo: "DE", Providers: []string{"acme", "beta"}}
	for _, tc := range []struct {
		route Route
		want  string
	}{
		{DefaultRoute(), "any"},
		{Route{Strict: true, Countries: []Country{nl, de}}, "NL > DE:acme,beta"},
		{Route{Countries: []Country{nl, de}}, "NL > DE:acme,beta > any"},
		{Route{Fastest: true, Strict: true, Countries: []Country{nl, de}}, "fastest NL DE:acme,beta"},
		{Route{Fastest: true, Countries: []Country{nl}}, "fastest NL > any"},
	} {
		if got := tc.route.String(); got != tc.want {
			t.Errorf("String = %q, want %q", got, tc.want)
		}
	}
}
