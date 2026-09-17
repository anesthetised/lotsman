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

	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/store"
)

func userAdd(configPath string, args []string) error {
	name, profile, err := nameAndProfile("user add", args, true)
	if err != nil {
		return err
	}
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
	peer, err := e.store.AddPeer(name, profile, e.cfg.Subnet)
	if errors.Is(err, store.ErrExists) {
		return fmt.Errorf("%s already has profile %s; use `user show`", name, profile)
	}
	if err != nil {
		return err
	}
	if err := printClientConfig(e, peer); err != nil {
		return err
	}
	nudgeDaemon(e)
	return nil
}

func userShow(configPath string, args []string) error {
	name, profile, err := nameAndProfile("user show", args, true)
	if err != nil {
		return err
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	peer, err := e.store.Peer(name, profile)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", name, profile, err)
	}
	return printClientConfig(e, peer)
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
	fmt.Fprintln(w, "USER\tPROFILE\tADDRESS\tPUBLIC KEY\tCREATED")
	for _, p := range peers {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.User, p.Profile, p.IP, p.PublicKey, p.CreatedAt.Format("2006-01-02"))
	}
	return w.Flush()
}

func userRemove(configPath string, args []string) error {
	name, profile, err := nameAndProfile("user rm", args, false)
	if err != nil {
		return err
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	if profile == "" {
		err = e.store.DeleteUser(name)
	} else {
		err = e.store.DeletePeer(name, profile)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	nudgeDaemon(e)
	return nil
}

func nameAndProfile(cmd string, args []string, profileRequired bool) (name, profile string, err error) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.StringVar(&profile, "profile", "", "profile name from the config")
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		return "", "", fmt.Errorf("%s: NAME is required", cmd)
	}
	name = args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return "", "", err
	}
	if profileRequired && profile == "" {
		return "", "", fmt.Errorf("%s: -profile is required", cmd)
	}
	return name, profile, nil
}

func printClientConfig(e *env, peer store.Peer) error {
	m, err := e.clientMTU(context.Background())
	if err != nil {
		return err
	}
	fmt.Print(daemon.ClientConfig(e.cfg, e.id, peer, m).String())
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
