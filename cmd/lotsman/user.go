package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/mdp/qrterminal/v3"

	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/policy"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/web"
)

// userAdd creates a user without devices, for an operator who sets their
// rights before handing out their link; `device add` creates one too.
func userAdd(configPath string, args []string) error {
	name, err := userName("user add", args)
	if err != nil {
		return err
	}
	if len(args) > 1 {
		return fmt.Errorf("user add: unexpected argument %q; `device add USER/DEVICE` adds a device", args[1])
	}
	if strings.Contains(name, "/") {
		return errors.New("user add: NAME must not contain /, which separates USER/DEVICE")
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	if _, err := e.store.CreateUser(name); errors.Is(err, store.ErrExists) {
		return fmt.Errorf("user %s already exists", name)
	} else if err != nil {
		return err
	}
	return nil
}

func userList(configPath string) error {
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	users, err := e.store.ListUsers()
	if err != nil {
		return err
	}
	peers, err := e.store.ListPeers()
	if err != nil {
		return err
	}
	byUser := map[string][]store.Peer{}
	for _, p := range peers {
		byUser[p.User] = append(byUser[p.User], p)
	}
	// Live data comes from the daemon's status file; without a daemon the columns stay "-".
	live := map[int64]daemon.PeerStatus{}
	if st, err := readStatus(e.cfg.StateDir); err == nil {
		for _, ps := range st.Peers {
			live[ps.ID] = ps
		}
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// The route goes last: it has spaces, as LAST HANDSHAKE does.
	fmt.Fprintln(w, "USER\tDEVICE\tADDRESS\tUPSTREAM\tLAST HANDSHAKE\tCREATED\tROUTE")
	for _, u := range users {
		// A user without devices keeps their rights and link, so list them too.
		if len(byUser[u.Name]) == 0 {
			fmt.Fprintf(w, "%s\t-\t-\t-\t-\t-\t%s\n", u.Name, u.CreatedAt.Format("2006-01-02"))
		}
		for _, p := range byUser[u.Name] {
			upstream, handshake := "-", "-"
			if ps, ok := live[p.ID]; ok {
				upstream, handshake = ps.Upstream, ago(ps.LastHandshake)
				if upstream == "" {
					upstream = "blocked"
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.User, p.Device, p.IP, upstream, handshake, p.CreatedAt.Format("2006-01-02"), p.Route)
		}
	}
	return w.Flush()
}

// userRights prints NAME's rights, after changing those given as flags.
// The exits are checked against the config file, as routes are; the daemon
// moves the devices off exits no longer allowed when nudged.
func userRights(configPath string, args []string) error {
	fs := flag.NewFlagSet("user rights", flag.ContinueOnError)
	editRoutes := fs.Bool("edit-routes", true, "whether the user sets their devices' routes on their page")
	showProviders := fs.Bool("show-providers", true, "whether the user's page shows providers, or countries only")
	exits := fs.String("exits", "all", "the upstreams the user's devices may use, joined by commas, or all")
	name, err := userName("user rights", args)
	if err != nil {
		return err
	}
	if err := parseAfterName(fs, args); err != nil {
		return err
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	u, err := e.store.User(name)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	r, changed := u.Rights, false
	var exitsErr error
	fs.Visit(func(f *flag.Flag) {
		changed = true
		switch f.Name {
		case "edit-routes":
			r.EditRoutes = *editRoutes
		case "show-providers":
			r.ShowProviders = *showProviders
		case "exits":
			r.Exits, exitsErr = parseExits(*exits)
		}
	})
	if exitsErr != nil {
		return fmt.Errorf("user rights: -exits: %w", exitsErr)
	}
	if changed {
		if err := policy.ValidateExits(r.Exits, e.cfg.Upstreams); err != nil {
			return fmt.Errorf("user rights: %w", err)
		}
		if u, err = e.store.SetRights(name, r); err != nil {
			return err
		}
		nudgeDaemon(e)
	}
	exitList := "all"
	if len(u.Rights.Exits) > 0 {
		exitList = strings.Join(u.Rights.Exits, ",")
	}
	fmt.Printf("edit-routes     %t\nshow-providers  %t\nexits           %s\n", u.Rights.EditRoutes, u.Rights.ShowProviders, exitList)
	return nil
}

// parseExits reads -exits: upstream names joined by commas, or all.
func parseExits(s string) ([]string, error) {
	if s == "all" {
		return nil, nil
	}
	exits := strings.Split(s, ",")
	if slices.Contains(exits, "") {
		return nil, fmt.Errorf("%q: want upstream names joined by commas, or all", s)
	}
	return exits, nil
}

// userRemove drops the user, their devices and their link.
func userRemove(configPath string, args []string) error {
	name, err := userName("user rm", args)
	if err != nil {
		return err
	}
	if len(args) > 1 {
		return fmt.Errorf("user rm: unexpected argument %q; `device rm USER/DEVICE` removes one device", args[1])
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	if err := e.store.DeleteUser(name); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	nudgeDaemon(e)
	return nil
}

// userName is the NAME a user command starts with.
func userName(cmd string, args []string) (string, error) {
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		return "", fmt.Errorf("%s: NAME is required", cmd)
	}
	return args[0], nil
}

// parseAfterName parses the flags after a command's first argument. Flag
// parsing stops at the first positional argument, so whatever followed it
// would be silently ignored; it is refused instead.
func parseAfterName(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s: unexpected argument %q (flags go after %s as -flag value)", fs.Name(), fs.Arg(0), args[0])
	}
	return nil
}

// userLink issues NAME's link to their page in the web panel, replacing the
// one they had, or with -revoke ends it. The link is printed once, to
// stdout: only its hash is stored. Its address is the panel's public_url,
// read from the panel's config, or -url; without either only the token is
// printed. The token goes in the fragment, <public_url>/u#<token>, which
// browsers never send, so it stays out of proxy logs.
func userLink(configPath string, args []string) error {
	fs := flag.NewFlagSet("user link", flag.ContinueOnError)
	revoke := fs.Bool("revoke", false, "end the user's link instead of issuing one")
	base := fs.String("url", "", "the panel's public URL (default: public_url of the panel's config, see `web`)")
	name, err := userName("user link", args)
	if err != nil {
		return err
	}
	if err := parseAfterName(fs, args); err != nil {
		return err
	}
	origin := *base
	if origin == "" {
		origin = panelOrigin()
	} else if err := checkPanelURL(origin); err != nil {
		return fmt.Errorf("user link: -url: %w", err)
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	if *revoke {
		if err := e.store.RevokeLink(name); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}
	token, err := e.store.IssueLink(name)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if origin == "" {
		fmt.Println(token)
		fmt.Fprintln(os.Stderr, "lotsman: no public URL known (-url, or public_url in the panel's config); the page is <public_url>/u#<token>")
	} else {
		fmt.Println(strings.TrimRight(origin, "/") + "/u#" + token)
	}
	fmt.Fprintf(os.Stderr, "lotsman: the link is shown only this once and replaces %s's previous one; it gives their configs to whoever has it\n", name)
	return nil
}

// panelOrigin is the panel's public_url, or "" without a readable panel
// config.
func panelOrigin() string {
	cfg, err := web.LoadConfig(defaultWebConfigPath())
	if err != nil {
		return ""
	}
	return cfg.Origin()
}

func checkPanelURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q: want https://host[:port]; the panel is served at the root", s)
	}
	return nil
}

// printClientConfig writes the config to stdout so it can be redirected to a
// file; the optional QR code goes to stderr so it never ends up in that file.
func printClientConfig(e *env, peer store.Peer, m int, qr bool) error {
	conf := daemon.ClientConfig(e.cfg, e.id, peer, m).String()
	if _, err := io.WriteString(os.Stdout, conf); err != nil {
		return err
	}
	if qr {
		qrterminal.GenerateHalfBlock(conf, qrterminal.L, os.Stderr)
	}
	return nil
}

// nudgeDaemon asks a running daemon to reconcile now instead of on its next
// tick; the daemon does not reload its config for it. A missing pid file means
// no daemon. Any other failure is reported and harmless: the tick will pick
// the change up.
func nudgeDaemon(e *env) {
	data, err := os.ReadFile(pidPath(e.cfg.StateDir))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err == nil && pid <= 0 {
		err = errors.New("not a process id")
	}
	if err == nil {
		err = signalDaemon(pid)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "lotsman: daemon not nudged, the change applies on its next tick: %v\n", err)
	}
}
