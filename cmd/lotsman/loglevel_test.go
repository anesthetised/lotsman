package main

import (
	"flag"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"":      slog.LevelInfo,
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"DEBUG": slog.LevelDebug,
		"Warn":  slog.LevelWarn,
	} {
		if got, err := parseLogLevel(in); err != nil || got != want {
			t.Errorf("parseLogLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"verbose", "warning", "trace", "3", " debug", "debug "} {
		if _, err := parseLogLevel(in); err == nil || !strings.Contains(err.Error(), "invalid log level") {
			t.Errorf("parseLogLevel(%q) err = %v, want invalid log level", in, err)
		}
	}
}

// TestResolveLogLevel checks the source order: -log-level, then
// $LOTSMAN_LOG_LEVEL, then info.
func TestResolveLogLevel(t *testing.T) {
	resolve := func(t *testing.T, args ...string) (slog.Level, error) {
		t.Helper()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		v := fs.String("log-level", "", "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return resolveLogLevel(fs, *v)
	}

	t.Run("default", func(t *testing.T) {
		t.Setenv("LOTSMAN_LOG_LEVEL", "")
		if got, err := resolve(t); err != nil || got != slog.LevelInfo {
			t.Errorf("got %v, %v; want info", got, err)
		}
	})
	t.Run("env", func(t *testing.T) {
		t.Setenv("LOTSMAN_LOG_LEVEL", "debug")
		if got, err := resolve(t); err != nil || got != slog.LevelDebug {
			t.Errorf("got %v, %v; want debug", got, err)
		}
	})
	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv("LOTSMAN_LOG_LEVEL", "debug")
		if got, err := resolve(t, "-log-level", "error"); err != nil || got != slog.LevelError {
			t.Errorf("got %v, %v; want error", got, err)
		}
	})
	t.Run("valid flag ignores a bad env", func(t *testing.T) {
		t.Setenv("LOTSMAN_LOG_LEVEL", "bogus")
		if got, err := resolve(t, "-log-level", "warn"); err != nil || got != slog.LevelWarn {
			t.Errorf("got %v, %v; want warn", got, err)
		}
	})
	t.Run("bad env names the variable", func(t *testing.T) {
		t.Setenv("LOTSMAN_LOG_LEVEL", "bogus")
		if _, err := resolve(t); err == nil || !strings.Contains(err.Error(), "$LOTSMAN_LOG_LEVEL") {
			t.Errorf("err = %v, want it to name $LOTSMAN_LOG_LEVEL", err)
		}
	})
	t.Run("bad flag names the flag", func(t *testing.T) {
		t.Setenv("LOTSMAN_LOG_LEVEL", "")
		if _, err := resolve(t, "-log-level", "bogus"); err == nil || !strings.Contains(err.Error(), "-log-level") {
			t.Errorf("err = %v, want it to name -log-level", err)
		}
	})
}

// TestCLILogLevel runs the real entry point: an invalid level must be
// rejected before any command starts, a valid one must not get in the way.
func TestCLILogLevel(t *testing.T) {
	cfg := testConfig(t)

	t.Run("invalid flag", func(t *testing.T) {
		t.Setenv("LOTSMAN_LOG_LEVEL", "")
		for _, args := range [][]string{{"serve"}, {"version"}, {"config", "check"}} {
			_, err := capture(t, append([]string{"-config", cfg, "-log-level", "verbose"}, args...)...)
			if err == nil || !strings.Contains(err.Error(), "invalid log level") {
				t.Errorf("%v: err = %v, want invalid log level", args, err)
			}
		}
	})
	t.Run("invalid env", func(t *testing.T) {
		t.Setenv("LOTSMAN_LOG_LEVEL", "verbose")
		for _, args := range [][]string{{"serve"}, {"version"}, {"config", "check"}} {
			_, err := capture(t, append([]string{"-config", cfg}, args...)...)
			if err == nil || !strings.Contains(err.Error(), "invalid log level") {
				t.Errorf("%v: err = %v, want invalid log level", args, err)
			}
		}
	})
	t.Run("valid", func(t *testing.T) {
		t.Setenv("LOTSMAN_LOG_LEVEL", "")
		for _, level := range []string{"debug", "info", "warn", "error", "DEBUG"} {
			if _, err := capture(t, "-config", cfg, "-log-level", level, "version"); err != nil {
				t.Errorf("-log-level %s: %v", level, err)
			}
		}
		t.Setenv("LOTSMAN_LOG_LEVEL", "warn")
		if _, err := capture(t, "-config", cfg, "version"); err != nil {
			t.Errorf("$LOTSMAN_LOG_LEVEL=warn: %v", err)
		}
	})
	t.Run("after the command it is rejected like -config", func(t *testing.T) {
		t.Setenv("LOTSMAN_LOG_LEVEL", "")
		_, err := capture(t, "-config", cfg, "serve", "-log-level", "debug")
		if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
			t.Errorf("err = %v, want unexpected argument", err)
		}
	})
}
