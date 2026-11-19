// Command lotsman is the AmneziaWG gateway daemon and its admin CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/mtu"
	"github.com/anesthetised/lotsman/internal/proxy/helper"
	"github.com/anesthetised/lotsman/internal/store"
)

// version is set at build time from git (see justfile).
var version = "dev"

const usage = `Usage: lotsman [-config FILE] [-log-level LEVEL] <command> [args]

Commands:
  version                     print the build version
  serve                       run the gateway (Linux, root)
  config check                validate the config, the upstream configs and the profiles
  profile list                list profiles, their peers and preference tiers
  profile add NAME [TIER...]  create profile NAME; each TIER is one preference, best first
  profile set NAME [TIER...]  replace the tiers of profile NAME
  profile rm NAME             remove a profile no peer uses
  profile import FILE         copy the profiles: key of an older lotsman.yaml into the store
  user add NAME -profile P [-device D] [-qr]
                              create a config for NAME's device D (default "default") with profile P
  user show NAME -profile P [-device D] [-qr]
                              print the config again
  user list                   list users, devices and profiles
  user rm NAME [-device D] [-profile P]
                              remove one config, every config of a device, or the whole user
  upstream status             show upstream health as last written by the daemon
  web                         run the web panel (as the lotsman-web user)

A TIER is key=value pairs joined by commas, with keys geo, provider and tag, all of
which an upstream must match (geo=NL,provider=acme), or "any". No TIER means any upstream.

Config file: -config, or $LOTSMAN_CONFIG, or /etc/lotsman/lotsman.yaml
  for web: -config, or $LOTSMAN_WEB_CONFIG, or /etc/lotsman/web.yaml
Log level (serve, web): -log-level, or $LOTSMAN_LOG_LEVEL, or info; one of debug, info, warn, error
`

func main() {
	// The daemon starts this binary as a proxy upstream's helper process;
	// the command is not for operators, so usage does not list it.
	if len(os.Args) == 2 && os.Args[1] == helper.Subcommand {
		os.Exit(helper.Main())
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lotsman:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("lotsman", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	configPath := fs.String("config", defaultConfigPath(), "path to lotsman.yaml (web.yaml for web)")
	logLevel := fs.String("log-level", "", "log level: debug, info, warn or error (default $LOTSMAN_LOG_LEVEL, then info)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	level, err := resolveLogLevel(fs, *logLevel)
	if err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("no command")
	}
	cmd := rest[0]
	if len(rest) > 1 && (cmd == "user" || cmd == "profile" || cmd == "config" || cmd == "upstream") {
		cmd += " " + rest[1]
		rest = rest[1:]
	}
	rest = rest[1:]

	switch cmd {
	case "version", "serve", "config check", "user list", "profile list", "upstream status", "web":
		if len(rest) > 0 {
			return fmt.Errorf("%s: unexpected argument %q", cmd, rest[0])
		}
	}

	switch cmd {
	case "version":
		fmt.Println(version)
		return nil
	case "serve":
		return serve(context.Background(), *configPath, level)
	case "config check":
		return checkConfig(*configPath)
	case "user add":
		return userAdd(*configPath, rest)
	case "user show":
		return userShow(*configPath, rest)
	case "user list":
		return userList(*configPath)
	case "user rm":
		return userRemove(*configPath, rest)
	case "profile list":
		return profileList(*configPath)
	case "profile add", "profile set":
		return profilePut(*configPath, cmd, rest)
	case "profile rm":
		return profileRemove(*configPath, rest)
	case "profile import":
		return profileImport(*configPath, rest)
	case "upstream status":
		return upstreamStatus(*configPath)
	case "web":
		// The panel has its own config: lotsman.yaml is root's alone.
		path := defaultWebConfigPath()
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "config" {
				path = *configPath
			}
		})
		return runWeb(context.Background(), path, level)
	}
	fs.Usage()
	return fmt.Errorf("unknown command %q", cmd)
}

func defaultConfigPath() string {
	if p := os.Getenv("LOTSMAN_CONFIG"); p != "" {
		return p
	}
	return "/etc/lotsman/lotsman.yaml"
}

// resolveLogLevel picks the log level from -log-level, else $LOTSMAN_LOG_LEVEL,
// else info. It runs for every command, so a typo is reported at once rather
// than only when `serve` is finally started.
func resolveLogLevel(fs *flag.FlagSet, flagValue string) (slog.Level, error) {
	value, source := os.Getenv("LOTSMAN_LOG_LEVEL"), "$LOTSMAN_LOG_LEVEL"
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "log-level" {
			value, source = flagValue, "-log-level"
		}
	})
	level, err := parseLogLevel(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", source, err)
	}
	return level, nil
}

// parseLogLevel maps a level name to slog's. The empty string means the
// default, info.
func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid log level %q: want debug, info, warn or error", s)
}

// env is what every command needs: the config, the server identity and the store.
type env struct {
	cfg   *config.Config
	id    daemon.Identity
	store *store.Store
}

func openEnv(configPath string) (*env, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	return openState(cfg)
}

// openState loads the identity and opens the store of an already loaded config.
func openState(cfg *config.Config) (*env, error) {
	id, err := daemon.LoadOrCreateIdentity(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(dbPath(cfg.StateDir))
	if err != nil {
		return nil, err
	}
	return &env{cfg: cfg, id: id, store: st}, nil
}

// clientMTU loads the upstream configs to size client packets. It mirrors
// Daemon.ClientMTU without needing devices or DNS.
func (e *env) clientMTU() (int, error) {
	var ups []mtu.Upstream
	for _, u := range e.cfg.Upstreams {
		up, err := daemon.LoadUpstream(u, e.cfg.Health)
		if err != nil {
			return 0, err
		}
		ups = append(ups, up.MTULimit())
	}
	return mtu.Client(e.id.Params.S4, ups), nil
}
