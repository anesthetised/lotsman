package helper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
)

// maxLine caps a relayed line, so a runaway helper cannot flood the
// daemon's log with one huge record.
const maxLine = 4096

// relayLog copies a helper's stderr into log until EOF: a JSON record the
// helper logged keeps its level, message and attributes; anything else,
// such as a panic and its stack trace, is logged line by line as an error.
// It returns the first line of a panic or fatal runtime error, if any.
func relayLog(r io.Reader, log *slog.Logger) (crash string) {
	br := bufio.NewReaderSize(r, maxLine)
	for {
		line, err := readLine(br)
		if line != "" {
			if level, msg, attrs, ok := parseRecord(line); ok {
				log.Log(context.Background(), level, msg, attrs...)
			} else {
				log.Error("proxy helper output", "line", line)
				if crash == "" && (strings.HasPrefix(line, "panic: ") || strings.HasPrefix(line, "fatal error: ")) {
					crash = line
				}
			}
		}
		if err != nil {
			return crash
		}
	}
}

// readLine returns the next line without its newline, cut to maxLine bytes.
func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadSlice('\n')
	if err != bufio.ErrBufferFull {
		return string(bytes.TrimSuffix(line, []byte("\n"))), err
	}
	s := string(line) + "…"
	for err == bufio.ErrBufferFull {
		_, err = br.ReadSlice('\n')
	}
	return s, err
}

// parseRecord reads a line slog's JSON handler wrote. The time is dropped,
// since the daemon's log adds its own; the other attributes keep their
// order.
func parseRecord(line string) (level slog.Level, msg string, attrs []any, ok bool) {
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return 0, "", nil, false
	}
	var haveLevel, haveMsg bool
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return 0, "", nil, false
		}
		key, _ := t.(string)
		var value any
		if err := dec.Decode(&value); err != nil {
			return 0, "", nil, false
		}
		switch s, isString := value.(string); {
		case key == slog.TimeKey:
		case key == slog.LevelKey && isString:
			haveLevel = level.UnmarshalText([]byte(s)) == nil
		case key == slog.MessageKey && isString:
			msg, haveMsg = s, true
		default:
			attrs = append(attrs, slog.Any(key, value))
		}
	}
	return level, msg, attrs, haveLevel && haveMsg
}
