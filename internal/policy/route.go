package policy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/anesthetised/lotsman/internal/config"
)

// Route is which exits a device's traffic uses, among its user's allowed
// exits. It lives on the gateway only: changing it never touches the client
// config or the tunnel. The store keeps it as JSON, read and written whole.
type Route struct {
	// Fastest false: the first country in order with an available exit
	// wins. True: the listed countries are one choice and the lowest
	// latency wins.
	Fastest bool `json:"fastest"`
	// Strict true: when no listed country has an available exit the device
	// is blocked. False: it falls back to any allowed exit.
	Strict    bool      `json:"strict"`
	Countries []Country `json:"countries"` // empty: any allowed exit
}

// Country selects the upstreams of one geo, of the given providers or, with
// none, of every provider.
type Country struct {
	Geo       string   `json:"geo"`
	Providers []string `json:"providers"`
}

// DefaultRoute is the route of a device added without one: the fastest of
// any allowed exit.
func DefaultRoute() Route { return Route{Fastest: true, Strict: true} }

// Tiers compiles the route into Select's preference tiers: one per country
// in order, or a single one of every country when Fastest, then a tier of
// any upstream unless Strict. A route without countries is that one tier.
func (r Route) Tiers() []Tier {
	if len(r.Countries) == 0 {
		return []Tier{{{}}}
	}
	var tiers []Tier
	for _, c := range r.Countries {
		tiers = append(tiers, c.tier())
	}
	if r.Fastest {
		tiers = []Tier{slices.Concat(tiers...)}
	}
	if !r.Strict {
		tiers = append(tiers, Tier{{}})
	}
	return tiers
}

func (c Country) tier() Tier {
	if len(c.Providers) == 0 {
		return Tier{{Geo: c.Geo}}
	}
	t := make(Tier, len(c.Providers))
	for i, p := range c.Providers {
		t[i] = Match{Geo: c.Geo, Provider: p}
	}
	return t
}

// Validate checks a route being written against the upstreams the user's
// exits allow (see Allows): every country must have one of them, every
// provider one in that country, and nothing may be listed twice, so a typo
// is refused rather than stored. A stored route may still go stale later,
// after an upstream is removed; it then simply matches less.
func (r Route) Validate(upstreams []config.Upstream, exits []string) error {
	what := "upstream"
	if len(exits) > 0 {
		what = "allowed exit"
	}
	var allowed []config.Upstream
	for _, u := range upstreams {
		if Allows(exits, u.Name) {
			allowed = append(allowed, u)
		}
	}
	for i, c := range r.Countries {
		if slices.ContainsFunc(r.Countries[:i], func(o Country) bool { return o.Geo == c.Geo }) {
			return fmt.Errorf("route: %q is listed twice", c.Geo)
		}
		// Exact labels, not Match: an empty geo or provider would match any.
		if !slices.ContainsFunc(allowed, func(u config.Upstream) bool { return u.Geo == c.Geo }) {
			return fmt.Errorf("route: no %s in %q", what, c.Geo)
		}
		for j, p := range c.Providers {
			if slices.Contains(c.Providers[:j], p) {
				return fmt.Errorf("route: provider %q is listed twice in %q", p, c.Geo)
			}
			if !slices.ContainsFunc(allowed, func(u config.Upstream) bool { return u.Geo == c.Geo && u.Provider == p }) {
				return fmt.Errorf("route: no %s of provider %q in %q", what, p, c.Geo)
			}
		}
	}
	return nil
}

// String writes the route for people: "NL > DE:acme,beta" is in order,
// "fastest NL DE" by latency, "> any" ends a route that falls back, and a
// route without countries is "any". Each country is written as the CLI takes
// it.
func (r Route) String() string {
	if len(r.Countries) == 0 {
		return "any"
	}
	countries := make([]string, len(r.Countries))
	for i, c := range r.Countries {
		countries[i] = c.String()
	}
	s := strings.Join(countries, " > ")
	if r.Fastest {
		s = "fastest " + strings.Join(countries, " ")
	}
	if !r.Strict {
		s += " > any"
	}
	return s
}

// String is the country as the CLI takes it: NL, or DE:acme,beta with
// providers.
func (c Country) String() string {
	if len(c.Providers) == 0 {
		return c.Geo
	}
	return c.Geo + ":" + strings.Join(c.Providers, ",")
}
