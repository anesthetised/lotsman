package policy

import (
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
)

func up(name, geo string, healthy bool, latency time.Duration) Upstream {
	return Upstream{
		Upstream: config.Upstream{Name: name, Geo: geo, Provider: "rs"},
		Healthy:  healthy,
		Latency:  latency,
	}
}

func TestSelect(t *testing.T) {
	nl := config.Profile{Name: "nl", Prefer: []config.Match{{Geo: "NL"}}}
	eu := config.Profile{Name: "eu", Prefer: []config.Match{{Geo: "NL"}, {Geo: "DE"}}}
	auto := config.Profile{Name: "auto"}

	cases := []struct {
		name      string
		profile   config.Profile
		upstreams []Upstream
		current   string
		want      string
		wantOK    bool
	}{
		{"nothing healthy fails closed", nl,
			[]Upstream{up("nl1", "NL", false, 0)}, "", "", false},
		{"no upstream matches profile fails closed", nl,
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
		{"auto matches everything", auto,
			[]Upstream{up("de1", "DE", true, 30), up("nl1", "NL", true, 10)}, "", "nl1", true},
		{"auto is sticky too", auto,
			[]Upstream{up("de1", "DE", true, 30), up("nl1", "NL", true, 10)}, "de1", "de1", true},
		{"current outside profile is not kept", nl,
			[]Upstream{up("de1", "DE", true, 0), up("nl1", "NL", true, 0)}, "de1", "nl1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Select(c.profile, c.upstreams, c.current)
			if got != c.want || ok != c.wantOK {
				t.Errorf("Select = (%q, %v), want (%q, %v)", got, ok, c.want, c.wantOK)
			}
		})
	}
}
