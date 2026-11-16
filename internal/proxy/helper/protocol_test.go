package helper

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/proxy"
)

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	sent := request{Op: opOpen, Spec: &proxy.Spec{Key: "ss://k", Server: netip.MustParseAddrPort("192.0.2.1:8388"), MaxFlows: 7}, LogLevel: slog.LevelWarn}
	if err := writeFrame(&buf, sent); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(&buf, request{Op: opServer, Server: netip.MustParseAddrPort("192.0.2.2:8388")}); err != nil {
		t.Fatal(err)
	}
	var got request
	if err := readFrame(&buf, &got); err != nil || got.Op != opOpen || *got.Spec != *sent.Spec || got.LogLevel != slog.LevelWarn {
		t.Fatalf("first frame = %+v, %v", got, err)
	}
	got = request{}
	if err := readFrame(&buf, &got); err != nil || got.Server != netip.MustParseAddrPort("192.0.2.2:8388") || got.Spec != nil {
		t.Fatalf("second frame = %+v, %v", got, err)
	}
	// Closed between frames and inside one.
	if err := readFrame(&buf, &got); err != io.EOF {
		t.Errorf("read at the end = %v, want EOF", err)
	}
	writeFrame(&buf, response{Error: "x"})
	buf.Truncate(buf.Len() - 1)
	if err := readFrame(&buf, &got); err != io.ErrUnexpectedEOF {
		t.Errorf("read of a cut frame = %v, want unexpected EOF", err)
	}

	// Neither side sends or accepts a frame over the limit.
	if err := writeFrame(io.Discard, request{Op: strings.Repeat("x", maxFrame)}); err == nil {
		t.Error("wrote an oversized frame")
	}
	huge := binary.BigEndian.AppendUint32(nil, maxFrame+1)
	if err := readFrame(bytes.NewReader(huge), &got); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("read an oversized frame: %v", err)
	}
}

func TestParseRecord(t *testing.T) {
	level, msg, attrs, ok := parseRecord(`{"time":"2026-11-16T10:00:00Z","level":"WARN","msg":"tcp dial","target":"198.51.100.7:443","n":3,"err":"refused"}`)
	if !ok || level != slog.LevelWarn || msg != "tcp dial" {
		t.Fatalf("parse = %v %q %v %v", level, msg, attrs, ok)
	}
	var keys []string
	for _, a := range attrs {
		keys = append(keys, a.(slog.Attr).Key)
	}
	if strings.Join(keys, ",") != "target,n,err" {
		t.Errorf("attributes %v, want in order and without time", keys)
	}
	for _, line := range []string{"panic: boom", "goroutine 1 [running]:", `{"msg":"no level"}`, `{"level":"LOUD","msg":"x"}`, `{"level":"INFO"}`, `{"level":"INFO","msg":"cut short…`} {
		if _, _, _, ok := parseRecord(line); ok {
			t.Errorf("%q parsed as a record", line)
		}
	}
}

func TestReadLineCaps(t *testing.T) {
	long := strings.Repeat("a", 3*maxLine)
	br := bufio.NewReaderSize(strings.NewReader("short\n"+long+"\nlast"), maxLine)
	var lines []string
	for {
		line, err := readLine(br)
		lines = append(lines, line)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			break
		}
	}
	if len(lines) != 3 || lines[0] != "short" || lines[2] != "last" {
		t.Fatalf("lines = %d: %q, %q", len(lines), lines[0], lines[len(lines)-1])
	}
	if want := strings.Repeat("a", maxLine) + "…"; lines[1] != want {
		t.Errorf("long line is %d bytes, want cut to %d", len(lines[1]), len(want))
	}
}
