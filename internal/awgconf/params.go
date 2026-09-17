package awgconf

import (
	"fmt"
	"strconv"
	"strings"
)

// Range is an AmneziaWG 2.0 header value: a single number or an inclusive "lo-hi" range.
type Range struct {
	Lo, Hi uint32
}

func ParseRange(s string) (Range, error) {
	lo, hi, found := strings.Cut(s, "-")
	if !found {
		hi = lo
	}
	l, err := strconv.ParseUint(strings.TrimSpace(lo), 10, 32)
	if err != nil {
		return Range{}, fmt.Errorf("range %q: %w", s, err)
	}
	h, err := strconv.ParseUint(strings.TrimSpace(hi), 10, 32)
	if err != nil {
		return Range{}, fmt.Errorf("range %q: %w", s, err)
	}
	if l > h {
		return Range{}, fmt.Errorf("range %q: lower bound above upper", s)
	}
	return Range{Lo: uint32(l), Hi: uint32(h)}, nil
}

func (r Range) String() string {
	if r.Lo == r.Hi {
		return strconv.FormatUint(uint64(r.Lo), 10)
	}
	return fmt.Sprintf("%d-%d", r.Lo, r.Hi)
}

func (r Range) IsZero() bool { return r == Range{} }

// Params are the AmneziaWG obfuscation parameters. Zero values mean "not set",
// which amneziawg-go treats as plain WireGuard behaviour for that parameter.
type Params struct {
	Jc, Jmin, Jmax uint32
	S1, S2, S3, S4 uint16
	H1, H2, H3, H4 Range
	I              [5]string
}

// UAPI renders the parameters as amneziawg-go device configuration lines.
func (p Params) UAPI() string {
	var b strings.Builder
	line := func(k string, v any) { fmt.Fprintf(&b, "%s=%v\n", k, v) }
	if p.Jc != 0 || p.Jmin != 0 || p.Jmax != 0 {
		line("jc", p.Jc)
		line("jmin", p.Jmin)
		line("jmax", p.Jmax)
	}
	line("s1", p.S1)
	line("s2", p.S2)
	line("s3", p.S3)
	line("s4", p.S4)
	for i, r := range [4]Range{p.H1, p.H2, p.H3, p.H4} {
		if !r.IsZero() {
			line(fmt.Sprintf("h%d", i+1), r)
		}
	}
	for i, v := range p.I {
		if v != "" {
			line(fmt.Sprintf("i%d", i+1), v)
		}
	}
	return b.String()
}
