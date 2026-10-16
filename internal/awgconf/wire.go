package awgconf

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Handshake message sizes in the pinned amneziawg-go (device/noise-protocol.go).
// The S1-S3 padding is prepended to each message in one UDP datagram.
const (
	initiationSize  = 148
	responseSize    = 92
	cookieReplySize = 64
)

// CheckWireSizes rejects S1-S3 padding and I1-I5 chains that would exceed
// payloadMax, the UDP payload limit of the peer's address family, or make the
// pinned sender allocate a negative or oversized buffer. It never allocates
// by the configured sizes.
func (p Params) CheckWireSizes(payloadMax int) error {
	for _, s := range []struct {
		name    string
		padding uint16
		message int
		what    string
	}{
		{"S1", p.S1, initiationSize, "handshake initiation"},
		{"S2", p.S2, responseSize, "handshake response"},
		{"S3", p.S3, cookieReplySize, "cookie reply"},
	} {
		if int(s.padding)+s.message > payloadMax {
			return fmt.Errorf("[Interface] %s plus the %d-byte %s must not exceed %d", s.name, s.message, s.what, payloadMax)
		}
	}
	for i, spec := range p.I {
		if err := checkSignature(spec, payloadMax); err != nil {
			return fmt.Errorf("[Interface] I%d %w", i+1, err)
		}
	}
	return nil
}

// checkSignature mirrors newObfChain in the pinned amneziawg-go. SendHandshakeInitiation
// allocates one buffer of the chain's summed length per nonnil chain, so the
// tag sizes must be nonnegative and sum to at most payloadMax. A string the
// runtime ignores (no tags) or refuses to build (any malformed tag) allocates
// nothing and stays accepted.
func checkSignature(spec string, payloadMax int) error {
	var sizes []int
	for {
		start := strings.IndexByte(spec, '<')
		if start < 0 {
			break
		}
		end := strings.IndexByte(spec[start:], '>')
		if end < 0 {
			return nil
		}
		end += start
		parts := strings.Fields(spec[start+1 : end])
		spec = spec[end+1:]
		if len(parts) == 0 {
			return nil
		}
		val := ""
		if len(parts) > 1 {
			val = parts[1]
		}
		var n int
		switch parts[0] {
		case "t":
			n = 4
		case "d", "ds":
		case "b":
			b, err := hex.DecodeString(strings.TrimPrefix(val, "0x"))
			if err != nil || len(b) == 0 {
				return nil
			}
			n = len(b)
		case "r", "rc", "rd", "dz":
			var err error
			if n, err = strconv.Atoi(val); err != nil {
				return nil
			}
		default:
			return nil
		}
		sizes = append(sizes, n)
	}
	// Each size is checked against the remaining room, so the sum cannot overflow.
	total := 0
	for _, n := range sizes {
		if n < 0 {
			return fmt.Errorf("has a negative tag size %d", n)
		}
		if n > payloadMax-total {
			return fmt.Errorf("packet size must not exceed %d", payloadMax)
		}
		total += n
	}
	return nil
}
