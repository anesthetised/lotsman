package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/store"
)

// Profiles live in the store. Each tier is one argument, so the order on the
// command line is the order of preference and no quoting is needed:
//
//	lotsman profile add eu tag=near geo=DE,provider=acme any
//
// A tier is key=value pairs joined by commas, keys geo, provider and tag, all
// of which an upstream must match; `any` matches every upstream. No tiers at
// all means any upstream. `profile list` prints tiers the same way.

func profileList(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	profiles, used, err := storedProfiles(cfg.StateDir)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROFILE\tPEERS\tPREFER")
	for _, p := range profiles {
		fmt.Fprintf(w, "%s\t%d\t%s\n", p.Name, used[p.Name], formatTiers(p.Prefer))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	printWarnings(cfg.Warnings(profiles))
	printWarnings(missingProfiles(profiles, used))
	return nil
}

// profilePut is `profile add` (create) and `profile set` (replace).
func profilePut(configPath, cmd string, args []string) error {
	p, err := parseProfileArgs(cmd, args)
	if err != nil {
		return err
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	// The CLI validates against the config file, as `user add` does; the
	// daemon checks its stored profiles again on every reload.
	if err := e.cfg.ValidateProfileWrite(p); err != nil {
		return err
	}
	put, hint := e.store.AddProfile, "names are unique ignoring case; use `profile set` to change it"
	if cmd == "profile set" {
		put, hint = e.store.SetProfile, "use `profile add` to create it"
	}
	if err := put(p); errors.Is(err, store.ErrExists) || errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("profile %q: %w; %s", p.Name, err, hint)
	} else if err != nil {
		return err
	}
	return profileChanged(e)
}

func profileRemove(configPath string, args []string) error {
	if len(args) != 1 || args[0] == "" || args[0][0] == '-' {
		return errors.New("profile rm: want exactly one NAME")
	}
	name := args[0]
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	err = e.store.DeleteProfile(name)
	if errors.Is(err, store.ErrInUse) {
		return fmt.Errorf("profile %q is in use; `user list` shows its peers, remove them with `user rm` first", name)
	}
	if err != nil {
		return fmt.Errorf("profile %q: %w", name, err)
	}
	return nil
}

// profileRename renames a profile and moves its peers to the new name. Their
// clients keep working: a peer's keys and address do not change.
func profileRename(configPath string, args []string) error {
	if len(args) != 2 || slices.ContainsFunc(args, func(a string) bool { return a == "" || a[0] == '-' }) {
		return errors.New("profile rename: want OLD NEW")
	}
	from, to := args[0], args[1]
	if err := config.ValidateProfileName(to); err != nil {
		return err
	}
	e, err := openEnv(configPath)
	if err != nil {
		return err
	}
	defer e.store.Close()
	err = e.store.RenameProfile(from, to)
	if errors.Is(err, store.ErrExists) {
		return fmt.Errorf("profile %q: the name is taken, ignoring case; see `profile list`", to)
	}
	if err != nil {
		return fmt.Errorf("profile %q: %w", from, err)
	}
	nudgeDaemon(e)
	return nil
}

// profileImport copies the profiles: key of a config from before profiles
// moved to the store. The config to validate them against may still have
// the key too: then FILE is usually that config itself.
func profileImport(configPath string, args []string) error {
	if len(args) != 1 || args[0] == "" || args[0][0] == '-' {
		return errors.New("profile import: want exactly one FILE, a lotsman.yaml with a profiles: key")
	}
	cfg, _, err := config.LoadLegacy(configPath)
	if err != nil {
		return err
	}
	_, profiles, err := config.LoadLegacy(args[0])
	if err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	if len(profiles) == 0 {
		return fmt.Errorf("%s has no profiles to import", args[0])
	}
	// All or nothing on validation, so a typo never imports half the file.
	for _, p := range profiles {
		if err := cfg.ValidateProfile(p); err != nil {
			return fmt.Errorf("%s: %w", args[0], err)
		}
	}
	e, err := openState(cfg)
	if err != nil {
		return err
	}
	defer e.store.Close()
	for _, p := range profiles {
		// Running the import again skips what it already stored.
		if err := e.store.AddProfile(p); errors.Is(err, store.ErrExists) {
			fmt.Fprintf(os.Stderr, "profile %s is already in the store, skipped\n", p.Name)
		} else if err != nil {
			return err
		} else {
			fmt.Printf("profile %s imported\n", p.Name)
		}
	}
	if err := profileChanged(e); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "now delete the profiles: key from %s\n", args[0])
	return nil
}

// profileChanged reports the warnings of the stored profiles and asks the
// daemon to reconcile, which reroutes the peers of a changed profile.
func profileChanged(e *env) error {
	profiles, err := e.store.ListProfiles()
	if err != nil {
		return err
	}
	printWarnings(e.cfg.Warnings(profiles))
	nudgeDaemon(e)
	return nil
}

// missingProfiles warns about peers whose profile is not in the store, as
// after an upgrade from a config with profiles: when nothing was imported.
func missingProfiles(profiles []config.Profile, used map[string]int) []string {
	var out []string
	for name, n := range used {
		if !slices.ContainsFunc(profiles, func(p config.Profile) bool { return p.Name == name }) {
			out = append(out, fmt.Sprintf("%d peers use profile %q, which is not in the store, and stay blocked; add it with `profile add` or `profile import`", n, name))
		}
	}
	slices.Sort(out)
	return out
}

func printWarnings(warnings []string) {
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
}

func parseProfileArgs(cmd string, args []string) (config.Profile, error) {
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		return config.Profile{}, fmt.Errorf("%s: NAME is required", cmd)
	}
	p := config.Profile{Name: args[0]}
	for _, arg := range args[1:] {
		m, err := parseTier(arg)
		if err != nil {
			return config.Profile{}, fmt.Errorf("%s: %w", cmd, err)
		}
		p.Prefer = append(p.Prefer, m)
	}
	return p, nil
}

// parseTier reads one tier: `any`, or key=value pairs joined by commas.
func parseTier(s string) (config.Match, error) {
	var m config.Match
	if s == "any" {
		return m, nil
	}
	for _, pair := range strings.Split(s, ",") {
		key, value, _ := strings.Cut(pair, "=")
		var field *string
		switch key {
		case "geo":
			field = &m.Geo
		case "provider":
			field = &m.Provider
		case "tag":
			field = &m.Tag
		default:
			return m, fmt.Errorf("tier %q: want key=value pairs with keys geo, provider and tag, joined by commas, or any", s)
		}
		if value == "" {
			return m, fmt.Errorf("tier %q: %s has no value", s, key)
		}
		if *field != "" {
			return m, fmt.Errorf("tier %q: %s is given twice", s, key)
		}
		*field = value
	}
	return m, nil
}

func formatTiers(prefer []config.Match) string {
	if len(prefer) == 0 {
		return "(any upstream)"
	}
	tiers := make([]string, len(prefer))
	for i, m := range prefer {
		tiers[i] = m.String() // parseTier's inverse
	}
	return strings.Join(tiers, " ")
}
