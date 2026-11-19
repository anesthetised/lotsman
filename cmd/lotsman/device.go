package main

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"

	"github.com/anesthetised/lotsman/internal/policy"
	"github.com/anesthetised/lotsman/internal/store"
)

// A device is one client config of a user, named USER/DEVICE, with its
// route. The route is the arguments after the flags, one country each, so
// the order on the command line is the order of preference:
//
//	lotsman device add alice/phone NL DE:acme,beta
//
// `user list` prints routes as policy.Route.String does.

func deviceAdd(configPath string, args []string) error {
	a, err := parseDeviceArgs("device add", args, true, true)
	if err != nil {
		return err
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	// Everything that can fail without touching the store runs first, so a
	// rejected command leaves neither a user nor a device behind.
	m, err := e.clientMTU()
	if err != nil {
		return err
	}
	rights := store.DefaultRights() // of a user this command creates
	if u, err := e.store.User(a.user); err == nil {
		rights = u.Rights
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err := a.route.Validate(e.cfg.Upstreams, rights.Exits); err != nil {
		return err
	}
	peer, err := e.store.AddUserPeer(a.user, a.device, a.route, e.cfg.Subnet)
	if errors.Is(err, store.ErrExists) {
		return fmt.Errorf("%s/%s already exists; use `device show` or `device route`", a.user, a.device)
	}
	if err != nil {
		return err
	}
	// The peer is committed: the daemon must learn about it even when the
	// config cannot be written.
	werr := printClientConfig(e, peer, m, a.qr)
	nudgeDaemon(e)
	if werr != nil {
		return fmt.Errorf("the device was added but its config was not written (%w); print it again with `device show %s/%s`", werr, a.user, a.device)
	}
	return nil
}

// deviceRoute gives a device a new route. Its key and address stay, so the
// client notices nothing but its traffic leaving elsewhere.
func deviceRoute(configPath string, args []string) error {
	a, err := parseDeviceArgs("device route", args, true, false)
	if err != nil {
		return err
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	u, err := e.store.User(a.user)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", a.user, a.device, err)
	}
	if err := a.route.Validate(e.cfg.Upstreams, u.Rights.Exits); err != nil {
		return err
	}
	if _, err := e.store.SetRoute(a.user, a.device, a.route); err != nil {
		return fmt.Errorf("%s/%s: %w", a.user, a.device, err)
	}
	nudgeDaemon(e)
	return nil
}

func deviceShow(configPath string, args []string) error {
	a, err := parseDeviceArgs("device show", args, false, true)
	if err != nil {
		return err
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	peer, err := e.store.Peer(a.user, a.device)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", a.user, a.device, err)
	}
	m, err := e.clientMTU()
	if err != nil {
		return err
	}
	return printClientConfig(e, peer, m, a.qr)
}

func deviceRemove(configPath string, args []string) error {
	a, err := parseDeviceArgs("device rm", args, false, false)
	if err != nil {
		return err
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	if err := e.store.DeletePeer(a.user, a.device); err != nil {
		return fmt.Errorf("%s/%s: %w", a.user, a.device, err)
	}
	nudgeDaemon(e)
	return nil
}

type deviceArgs struct {
	user, device string
	route        policy.Route
	qr           bool
}

// parseDeviceArgs reads USER/DEVICE, the flags, and with withRoute the
// route: -fastest, -fallback and the countries after them. Without
// countries the route is policy.DefaultRoute, whatever the flags.
func parseDeviceArgs(cmd string, args []string, withRoute, withQR bool) (deviceArgs, error) {
	var a deviceArgs
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	var fastest, fallback bool
	if withRoute {
		fs.BoolVar(&fastest, "fastest", false, "the lowest latency of every country wins, not the first in order")
		fs.BoolVar(&fallback, "fallback", false, "when no country has an upstream, use any instead of blocking the device")
	}
	if withQR {
		fs.BoolVar(&a.qr, "qr", false, "also print the config as a QR code on stderr")
	}
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		return a, fmt.Errorf("%s: USER/DEVICE is required", cmd)
	}
	var ok bool
	// A user name has no slash, so the first one separates them.
	if a.user, a.device, ok = strings.Cut(args[0], "/"); !ok || a.user == "" || a.device == "" {
		return a, fmt.Errorf("%s: want USER/DEVICE, got %q", cmd, args[0])
	}
	if err := fs.Parse(args[1:]); err != nil {
		return a, err
	}
	if !withRoute {
		if fs.NArg() > 0 {
			return a, fmt.Errorf("%s: unexpected argument %q (flags go after USER/DEVICE as -flag value)", cmd, fs.Arg(0))
		}
		return a, nil
	}
	a.route = policy.Route{Fastest: fastest, Strict: !fallback}
	for _, arg := range fs.Args() {
		c, err := parseCountry(arg)
		if err != nil {
			return a, fmt.Errorf("%s: %w", cmd, err)
		}
		a.route.Countries = append(a.route.Countries, c)
	}
	if len(a.route.Countries) == 0 {
		a.route = policy.DefaultRoute()
	}
	return a, nil
}

// parseCountry reads COUNTRY[:PROVIDER,...], policy.Country.String's
// inverse; the country may be given in lowercase.
func parseCountry(s string) (policy.Country, error) {
	if strings.HasPrefix(s, "-") {
		return policy.Country{}, fmt.Errorf("%q: flags go before the countries", s)
	}
	geo, providers, hasProviders := strings.Cut(s, ":")
	c := policy.Country{Geo: strings.ToUpper(geo)}
	if hasProviders {
		c.Providers = strings.Split(providers, ",")
	}
	if geo == "" || slices.Contains(c.Providers, "") {
		return policy.Country{}, fmt.Errorf("%q: want COUNTRY or COUNTRY:PROVIDER,PROVIDER", s)
	}
	return c, nil
}
