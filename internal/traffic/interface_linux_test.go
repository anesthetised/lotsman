package traffic

import "testing"

func TestInterfaceCounters(t *testing.T) {
	if _, _, err := InterfaceCounters("lo"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InterfaceCounters("lm-no-such"); err == nil {
		t.Error("no error for a missing interface")
	}
}

// BenchmarkInterfaceCounters is one upstream's share of a sample.
func BenchmarkInterfaceCounters(b *testing.B) {
	for b.Loop() {
		if _, _, err := InterfaceCounters("lo"); err != nil {
			b.Fatal(err)
		}
	}
}
