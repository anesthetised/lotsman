package policy

import (
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
)

// A tag groups upstreams from different countries into one tier that is picked
// by latency, which a geo or provider matcher alone cannot express.
func TestSelectTagTiersFromConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(`
endpoint: vpn.example.org:51820
upstreams:
  - {name: lt, conf: /lt, geo: LT, tags: [near, eu]}
  - {name: pl, conf: /pl, geo: PL, tags: [near, eu]}
  - {name: nl, conf: /nl, geo: NL, tags: [eu]}
  - {name: us, conf: /us, geo: US}
profiles:
  - {name: p, prefer: [{tag: near}, {tag: eu}]}
  - {name: both, prefer: [{tag: eu, geo: NL}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := cfg.Profile("p")
	both, _ := cfg.Profile("both")
	ups := func(lt, pl, nl, us bool) []Upstream {
		return []Upstream{
			{Upstream: cfg.Upstreams[0], Healthy: lt, Latency: 40 * time.Millisecond},
			{Upstream: cfg.Upstreams[1], Healthy: pl, Latency: 30 * time.Millisecond},
			{Upstream: cfg.Upstreams[2], Healthy: nl, Latency: 5 * time.Millisecond},
			{Upstream: cfg.Upstreams[3], Healthy: us, Latency: 1 * time.Millisecond},
		}
	}
	for _, tc := range []struct {
		name    string
		profile config.Profile
		ups     []Upstream
		want    string
		ok      bool
	}{
		{"lowest latency within the tag tier, not the nearest country", p, ups(true, true, true, true), "pl", true},
		{"tier member down, sibling takes over", p, ups(true, false, true, true), "lt", true},
		{"near tier down, falls to eu even if untagged is faster", p, ups(false, false, true, true), "nl", true},
		{"only untagged left: fail closed", p, ups(false, false, false, true), "", false},
		{"all given fields must match", both, ups(true, true, true, true), "nl", true},
		{"all given fields must match, down", both, ups(true, true, false, true), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Select(tc.profile, tc.ups, "")
			if got != tc.want || ok != tc.ok {
				t.Errorf("Select = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
