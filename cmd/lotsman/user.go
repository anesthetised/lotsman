package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/mdp/qrterminal/v3"

	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/web"
)

func userAdd(configPath string, args []string) error {
	a, err := parseUserArgs("user add", args, true)
	if err != nil {
		return err
	}
	name, device, profile := a.name, a.device, a.profile
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	// Everything that can fail without touching the store runs first, so a
	// rejected command leaves neither a user nor a peer behind.
	m, err := e.clientMTU()
	if err != nil {
		return err
	}
	peer, err := e.store.AddUserPeer(name, device, profile, e.cfg.Subnet)
	if errors.Is(err, store.ErrExists) {
		return fmt.Errorf("%s/%s already has profile %s; use `user show`", name, device, profile)
	}
	if errors.Is(err, store.ErrNoProfile) {
		return fmt.Errorf("profile %q does not exist; see `profile list`", profile)
	}
	if err != nil {
		return err
	}
	// The peer is committed: the daemon must learn about it even when the
	// config cannot be written.
	werr := printClientConfig(e, peer, m, a.qr)
	nudgeDaemon(e)
	if werr != nil {
		return fmt.Errorf("the peer was added but its config was not written (%w); print it again with `user show %s -profile %s -device %s`", werr, name, profile, device)
	}
	return nil
}

func userShow(configPath string, args []string) error {
	a, err := parseUserArgs("user show", args, true)
	if err != nil {
		return err
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	peer, err := e.store.Peer(a.name, a.device, a.profile)
	if err != nil {
		return fmt.Errorf("%s/%s/%s: %w", a.name, a.device, a.profile, err)
	}
	m, err := e.clientMTU()
	if err != nil {
		return err
	}
	return printClientConfig(e, peer, m, a.qr)
}

func userList(configPath string) error {
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	peers, err := e.store.ListPeers()
	if err != nil {
		return err
	}
	// Live data comes from the daemon's status file; without a daemon the columns stay "-".
	live := map[[3]string]daemon.PeerStatus{}
	if st, err := readStatus(e.cfg.StateDir); err == nil {
		for _, ps := range st.Peers {
			live[[3]string{ps.User, ps.Device, ps.Profile}] = ps
		}
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USER\tDEVICE\tPROFILE\tADDRESS\tUPSTREAM\tLAST HANDSHAKE\tCREATED")
	for _, p := range peers {
		upstream, handshake := "-", "-"
		if ps, ok := live[[3]string{p.User, p.Device, p.Profile}]; ok {
			upstream, handshake = ps.Upstream, ago(ps.LastHandshake)
			if upstream == "" {
				upstream = "blocked"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.User, p.Device, p.Profile, p.IP, upstream, handshake, p.CreatedAt.Format("2006-01-02"))
	}
	return w.Flush()
}

// userRemove drops one peer (-device and -profile), every profile of a
// device (-device only), or the whole user (neither).
func userRemove(configPath string, args []string) error {
	a, err := parseUserArgs("user rm", args, false)
	if err != nil {
		return err
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	switch {
	case a.profile != "":
		err = e.store.DeletePeer(a.name, a.device, a.profile)
	case a.deviceSet:
		err = e.store.DeleteDevice(a.name, a.device)
	default:
		err = e.store.DeleteUser(a.name)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", a.name, err)
	}
	nudgeDaemon(e)
	return nil
}

// userLink issues NAME's link to their page in the web panel, replacing the
// one they had, or with -revoke ends it. The link is printed once, to
// stdout: only its hash is stored. Its address is the panel's public_url,
// read from the panel's config, or -url; without either only the token is
// printed.
func userLink(configPath string, args []string) error {
	fs := flag.NewFlagSet("user link", flag.ContinueOnError)
	revoke := fs.Bool("revoke", false, "end the user's link instead of issuing one")
	base := fs.String("url", "", "the panel's public URL (default: public_url of the panel's config, see `web`)")
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		return errors.New("user link: NAME is required")
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("user link: unexpected argument %q (flags go after NAME as -flag value)", fs.Arg(0))
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
		fmt.Fprintln(os.Stderr, "lotsman: no public URL known (-url, or public_url in the panel's config); the page is <public_url>/u/<token>")
	} else {
		fmt.Println(strings.TrimRight(origin, "/") + "/u/" + token)
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

type userArgs struct {
	name, device, profile string
	deviceSet, qr         bool
}

func parseUserArgs(cmd string, args []string, profileRequired bool) (userArgs, error) {
	var a userArgs
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.StringVar(&a.profile, "profile", "", "profile name, see `profile list`")
	fs.StringVar(&a.device, "device", store.DefaultDevice, "the user's device; each device needs its own config")
	fs.BoolVar(&a.qr, "qr", false, "also print the config as a QR code on stderr")
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		return a, fmt.Errorf("%s: NAME is required", cmd)
	}
	a.name = args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return a, err
	}
	// Flag parsing stops at the first positional argument; whatever follows
	// would be silently ignored, e.g. `user rm alice phone -profile nl`.
	if fs.NArg() > 0 {
		return a, fmt.Errorf("%s: unexpected argument %q (flags go after NAME as -flag value)", cmd, fs.Arg(0))
	}
	var profileSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "device":
			a.deviceSet = true
		case "profile":
			profileSet = true
		}
	})
	// An empty value would pick the whole-user or whole-device form of
	// `user rm`, e.g. from an unset shell variable: `-profile "$P"`.
	if profileSet && a.profile == "" {
		return a, fmt.Errorf("%s: -profile must not be empty", cmd)
	}
	if profileRequired && a.profile == "" {
		return a, fmt.Errorf("%s: -profile is required", cmd)
	}
	return a, nil
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
