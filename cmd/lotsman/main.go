// Command lotsman is the AmneziaWG 2.0 gateway daemon and its admin CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/mtu"
	"github.com/anesthetised/lotsman/internal/store"
)

// version is set at build time from git (see justfile).
var version = "dev"

const usage = `Usage: lotsman [-config FILE] <command> [args]

Commands:
  version                     print the build version
  serve                       run the gateway (Linux, root)
  config check                validate the config and the upstream configs
  user add NAME -profile P [-device D] [-qr]
                              create a config for NAME's device D (default "default") with profile P
  user show NAME -profile P [-device D] [-qr]
                              print the config again
  user list                   list users, devices and profiles
  user rm NAME [-device D] [-profile P]
                              remove one config, every config of a device, or the whole user
  upstream status             show upstream health as last written by the daemon

Config file: -config, or $LOTSMAN_CONFIG, or /etc/lotsman/lotsman.yaml
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lotsman:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("lotsman", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	configPath := fs.String("config", defaultConfigPath(), "path to lotsman.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("no command")
	}
	cmd := rest[0]
	if len(rest) > 1 && (cmd == "user" || cmd == "config" || cmd == "upstream") {
		cmd += " " + rest[1]
		rest = rest[1:]
	}
	rest = rest[1:]

	switch cmd {
	case "version":
		fmt.Println(version)
		return nil
	case "serve":
		return serve(context.Background(), *configPath)
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
	case "upstream status":
		return upstreamStatus(*configPath)
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
	id, err := daemon.LoadOrCreateIdentity(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.StateDir, "lotsman.db"))
	if err != nil {
		return nil, err
	}
	return &env{cfg: cfg, id: id, store: st}, nil
}

// clientMTU loads the upstream configs to size client packets. It mirrors
// Daemon.ClientMTU without needing devices.
func (e *env) clientMTU(ctx context.Context) (int, error) {
	var ups []mtu.Upstream
	for _, u := range e.cfg.Upstreams {
		up, err := daemon.LoadUpstream(ctx, u, e.cfg.Health)
		if err != nil {
			return 0, err
		}
		ups = append(ups, mtu.Upstream{MTU: up.MTU, S4: up.Conf.Interface.S4})
	}
	return mtu.Client(e.id.Params.S4, ups), nil
}
