package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"text/tabwriter"

	"github.com/mdp/qrterminal/v3"

	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/store"
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
	if _, ok := e.cfg.Profile(profile); !ok {
		return fmt.Errorf("profile %q is not in the config", profile)
	}
	if _, err := e.store.CreateUser(name); err != nil && !errors.Is(err, store.ErrExists) {
		return err
	}
	peer, err := e.store.AddPeer(name, device, profile, e.cfg.Subnet)
	if errors.Is(err, store.ErrExists) {
		return fmt.Errorf("%s/%s already has profile %s; use `user show`", name, device, profile)
	}
	if err != nil {
		return err
	}
	if err := printClientConfig(e, peer, a.qr); err != nil {
		return err
	}
	nudgeDaemon(e)
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
	return printClientConfig(e, peer, a.qr)
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
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USER\tDEVICE\tPROFILE\tADDRESS\tPUBLIC KEY\tCREATED")
	for _, p := range peers {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.User, p.Device, p.Profile, p.IP, p.PublicKey, p.CreatedAt.Format("2006-01-02"))
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

type userArgs struct {
	name, device, profile string
	deviceSet, qr         bool
}

func parseUserArgs(cmd string, args []string, profileRequired bool) (userArgs, error) {
	var a userArgs
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.StringVar(&a.profile, "profile", "", "profile name from the config")
	fs.StringVar(&a.device, "device", store.DefaultDevice, "the user's device; each device needs its own config")
	fs.BoolVar(&a.qr, "qr", false, "also print the config as a QR code on stderr")
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		return a, fmt.Errorf("%s: NAME is required", cmd)
	}
	a.name = args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return a, err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "device" {
			a.deviceSet = true
		}
	})
	if profileRequired && a.profile == "" {
		return a, fmt.Errorf("%s: -profile is required", cmd)
	}
	return a, nil
}

// printClientConfig writes the config to stdout so it can be redirected to a
// file; the optional QR code goes to stderr so it never ends up in that file.
func printClientConfig(e *env, peer store.Peer, qr bool) error {
	m, err := e.clientMTU(context.Background())
	if err != nil {
		return err
	}
	conf := daemon.ClientConfig(e.cfg, e.id, peer, m).String()
	fmt.Print(conf)
	if qr {
		qrterminal.GenerateHalfBlock(conf, qrterminal.L, os.Stderr)
	}
	return nil
}

// nudgeDaemon asks a running daemon to reconcile now instead of on its next
// tick. Failure is fine: the tick will pick the change up.
func nudgeDaemon(e *env) {
	data, err := os.ReadFile(pidPath(e.cfg.StateDir))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		p.Signal(syscall.SIGHUP)
	}
}
