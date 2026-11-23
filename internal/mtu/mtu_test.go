package mtu

import "testing"

func TestClient(t *testing.T) {
	cases := []struct {
		name      string
		s4Down    uint16
		upstreams []Upstream
		want      int
	}{
		{"no upstreams, plain wireguard", 0, nil, 1420},
		{"no upstreams, S4 padding", 8, nil, 1412},
		{"one default upstream limits", 0, []Upstream{{MTU: 0}}, 1420},
		{"padding fits outside the upstream interface MTU", 12, []Upstream{{MTU: 1280, S4: 20}}, 1280},
		{"smaller upstream interface remains the limit", 12, []Upstream{{MTU: 1200, S4: 20}}, 1200},
		{"upstream outer budget leaves room for alignment", 0, []Upstream{{MTU: 1420, S4: 8}}, 1408},
		{"interface MTU caps alignment when it fits", 0, []Upstream{{MTU: 1412, S4: 8}}, 1412},
		{"smallest upstream wins", 0, []Upstream{{MTU: 1420}, {MTU: 1380, S4: 16}, {MTU: 1400}}, 1380},
		{"upstream content padding is subtracted", 0, []Upstream{{MTU: 1420, ContentPadding: 64}}, 1356},
		{"content padding adds to S4", 0, []Upstream{{MTU: 1420, S4: 12, ContentPadding: 64}}, 1344},
		{"content padding decides the smallest", 0, []Upstream{{MTU: 1380, S4: 16}, {MTU: 1420, ContentPadding: 100}}, 1320},
		{"proxy stack owns its outer framing", 0, []Upstream{{MTU: 1500, Proxy: true}}, 1420},
		{"downstream bound wins over roomy upstream", 100, []Upstream{{MTU: 1420}}, 1320},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Client(c.s4Down, c.upstreams); got != c.want {
				t.Errorf("Client(%d, %v) = %d, want %d", c.s4Down, c.upstreams, got, c.want)
			}
		})
	}
}
