// Package helper runs a proxy upstream's stack in a child process of the
// same binary, so a panic in gVisor or a proxy protocol takes down that
// upstream only, not the gateway.
//
// The daemon keeps the TUN and passes its descriptor to the helper as fd 3.
// Fd 4 is the control connection, one end of a socket pair: the daemon
// sends requests, the helper answers each in turn. A frame is a 4-byte
// big-endian length and a JSON object. The first request is "open", with
// the access key, the resolved server and the flow limit; then come
// "server" (the key text and the resolved server, for a moved server or a
// rotated key), "max_flows" and "stats". The daemon closes the connection to
// stop the helper. The helper logs JSON records to stderr, which the daemon
// relays into its own log. The key crosses only the control connection,
// never argv or the environment, which other processes can read.
package helper

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/netip"

	"github.com/anesthetised/lotsman/internal/proxy"
)

// Subcommand is the hidden command line that starts a helper.
const Subcommand = "proxy-helper"

// The descriptors a helper inherits beside stdin, stdout and stderr.
const (
	tunFD     = 3
	controlFD = 4
)

const (
	opOpen     = "open"
	opServer   = "server"
	opMaxFlows = "max_flows"
	opStats    = "stats"
)

// maxFrame bounds a frame, so a confused peer cannot make the other
// allocate without limit.
const maxFrame = 64 << 10

type request struct {
	Op       string         `json:"op"`
	Spec     *proxy.Spec    `json:"spec,omitempty"`      // open
	LogLevel slog.Level     `json:"log_level,omitempty"` // open
	Key      string         `json:"key,omitempty"`       // server: the key text, as in Spec
	Server   netip.AddrPort `json:"server,omitzero"`     // server
	MaxFlows int            `json:"max_flows,omitempty"` // max_flows
}

type response struct {
	Error string       `json:"error,omitempty"`
	Stats *proxy.Stats `json:"stats,omitempty"`
}

func writeFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > maxFrame {
		return fmt.Errorf("frame of %d bytes exceeds %d", len(body), maxFrame)
	}
	frame := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(body)), uint32(len(body)))
	_, err = w.Write(append(frame, body...))
	return err
}

// readFrame reads one frame into v. A connection closed between frames
// returns io.EOF; one closed inside a frame, io.ErrUnexpectedEOF.
func readFrame(r io.Reader, v any) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n > maxFrame {
		return fmt.Errorf("frame of %d bytes exceeds %d", n, maxFrame)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	return json.Unmarshal(body, v)
}
