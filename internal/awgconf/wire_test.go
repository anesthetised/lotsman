package awgconf

import (
	"strconv"
	"strings"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/device"
)

func TestHandshakeSizesMatchRuntime(t *testing.T) {
	if initiationSize != device.MessageInitiationSize || responseSize != device.MessageResponseSize || cookieReplySize != device.MessageCookieReplySize {
		t.Fatalf("sizes %d/%d/%d, runtime %d/%d/%d", initiationSize, responseSize, cookieReplySize,
			device.MessageInitiationSize, device.MessageResponseSize, device.MessageCookieReplySize)
	}
}

// These fixtures are only checked or parsed; none is passed to a device.
func TestCheckWireSizes(t *testing.T) {
	const maxInt = "9223372036854775807"
	for _, payloadMax := range []int{65507, 65527} {
		for _, tc := range []struct {
			name      string
			params    Params
			wantError string
		}{
			{"plain defaults", Params{}, ""},
			{"typical export", Params{S1: 15, S2: 25, S3: 10, I: [5]string{"<b 0xc000000001><r 64><t>", "<rd 12><rc 8>"}}, ""},
			{"S1 at limit", Params{S1: uint16(payloadMax - 148)}, ""},
			{"S2 at limit", Params{S2: uint16(payloadMax - 92)}, ""},
			{"S3 at limit", Params{S3: uint16(payloadMax - 64)}, ""},
			{"S1 over limit", Params{S1: uint16(payloadMax - 147)}, "S1 plus the 148-byte handshake initiation must not exceed"},
			{"S2 over limit", Params{S2: uint16(payloadMax - 91)}, "S2 plus the 92-byte handshake response must not exceed"},
			{"S3 over limit", Params{S3: uint16(payloadMax - 63)}, "S3 plus the 64-byte cookie reply must not exceed"},
			{"S1 maximum", Params{S1: 65535}, "S1 plus"},
			{"S2 maximum", Params{S2: 65535}, "S2 plus"},
			{"S3 maximum", Params{S3: 65535}, "S3 plus"},
			{"S4 is not a handshake padding", Params{S4: 65535}, ""},

			{"I at limit", Params{I: [5]string{"<r " + strconv.Itoa(payloadMax) + ">"}}, ""},
			{"I over limit", Params{I: [5]string{"<r " + strconv.Itoa(payloadMax+1) + ">"}}, "I1 packet size must not exceed"},
			{"I sum at limit", Params{I: [5]string{"", "<rc 1><b 0xabcd><t><rd " + strconv.Itoa(payloadMax-7) + ">"}}, ""},
			{"I sum over limit", Params{I: [5]string{"", "<rc 1><b 0xabcd><t><rd " + strconv.Itoa(payloadMax-6) + ">"}}, "I2 packet size must not exceed"},
			{"I dz at limit", Params{I: [5]string{"", "", "<d><ds><dz " + strconv.Itoa(payloadMax) + ">"}}, ""},
			{"I dz over limit", Params{I: [5]string{"", "", "<dz " + strconv.Itoa(payloadMax+1) + ">"}}, "I3 packet size must not exceed"},
			{"I many tags over limit", Params{I: [5]string{"", "", "", strings.Repeat("<r 1000>", payloadMax/1000+1)}}, "I4 packet size must not exceed"},
			{"I bytes over limit", Params{I: [5]string{"", "", "", "", "<b 0x" + strings.Repeat("ab", payloadMax+1) + ">"}}, "I5 packet size must not exceed"},
			{"I bytes at limit", Params{I: [5]string{"", "", "", "", "<b " + strings.Repeat("ab", payloadMax) + ">"}}, ""},
			{"I large value", Params{I: [5]string{"<r 2147483647>"}}, "I1 packet size must not exceed"},
			{"I maximum int", Params{I: [5]string{"<r " + maxInt + ">"}}, "I1 packet size must not exceed"},
			{"I sum overflow", Params{I: [5]string{"<r " + maxInt + "><rc " + maxInt + ">"}}, "I1 packet size must not exceed"},
			{"I negative r", Params{I: [5]string{"<r -1>"}}, "I1 has a negative tag size -1"},
			{"I negative rc", Params{I: [5]string{"<rc -1>"}}, "negative tag size"},
			{"I negative rd", Params{I: [5]string{"<rd -1>"}}, "negative tag size"},
			{"I negative dz", Params{I: [5]string{"<dz -1>"}}, "negative tag size"},
			{"I negative minimum int", Params{I: [5]string{"<r -9223372036854775808>"}}, "negative tag size"},
			{"I negative cancels positive", Params{I: [5]string{"<r 10><r -5>"}}, "negative tag size"},

			{"inert without tags", Params{I: [5]string{"ignored"}}, ""},
			{"inert unterminated", Params{I: [5]string{"<r -1"}}, ""},
			{"inert empty tag", Params{I: [5]string{"<>"}}, ""},
			{"inert unknown tag", Params{I: [5]string{"<x 5>"}}, ""},
			{"inert missing number", Params{I: [5]string{"<r>"}}, ""},
			{"inert bad number", Params{I: [5]string{"<r abc>"}}, ""},
			{"inert number out of range", Params{I: [5]string{"<r 99999999999999999999>"}}, ""},
			{"inert odd bytes", Params{I: [5]string{"<b 0x123>"}}, ""},
			{"inert empty bytes", Params{I: [5]string{"<b 0x>"}}, ""},
			{"inert negative beside unknown tag", Params{I: [5]string{"<r -1><x>"}}, ""},
			{"inert oversized beside bad tag", Params{I: [5]string{"<r 70000><r abc>"}}, ""},
		} {
			t.Run(strconv.Itoa(payloadMax)+"/"+tc.name, func(t *testing.T) {
				err := tc.params.CheckWireSizes(payloadMax)
				if tc.wantError == "" {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("CheckWireSizes error = %v, want %q", err, tc.wantError)
				}
			})
		}
	}
}

func TestParseWireSizes(t *testing.T) {
	key, err := ParseKey(privB64)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		params    Params
		wantError string
	}{
		{"maximum IPv6 S1", Params{S1: 65527 - 148}, ""},
		{"oversized S1", Params{S1: 65535}, "S1 plus the 148-byte handshake initiation must not exceed 65527"},
		{"maximum IPv6 I", Params{I: [5]string{"<r 65527>"}}, ""},
		{"negative I", Params{I: [5]string{"<r -1>"}}, "I1 has a negative tag size -1"},
		{"oversized I", Params{I: [5]string{"", "<r 2147483647>"}}, "I2 packet size must not exceed 65527"},
		{"inert I", Params{I: [5]string{"<r abc>"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := (&Config{Interface: Interface{PrivateKey: key, Params: tc.params}}).String()
			cfg, err := Parse(strings.NewReader(input))
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("Parse error = %v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Interface.Params != tc.params {
				t.Fatalf("params = %+v, want %+v", cfg.Interface.Params, tc.params)
			}
		})
	}
}
