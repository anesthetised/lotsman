package mtu

import "testing"

func TestClient(t *testing.T) {
	cases := []struct {
		name      string
		s4Down    uint16
		upstreams []Upstream
		want      int
	}{
		{"no upstreams, plain wireguard", 0, nil, 1440},
		{"no upstreams, S4 padding", 8, nil, 1432},
		{"one default upstream limits", 0, []Upstream{{MTU: 0}}, 1420},
		{"upstream S4 is subtracted", 0, []Upstream{{MTU: 1420, S4: 8}}, 1412},
		{"smallest upstream wins", 0, []Upstream{{MTU: 1420}, {MTU: 1380, S4: 16}, {MTU: 1400}}, 1364},
		{"downstream bound wins over roomy upstream", 100, []Upstream{{MTU: 1420}}, 1340},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Client(c.s4Down, c.upstreams); got != c.want {
				t.Errorf("Client(%d, %v) = %d, want %d", c.s4Down, c.upstreams, got, c.want)
			}
		})
	}
}
