package daemon

import (
	"errors"
	"fmt"

	"github.com/anesthetised/lotsman/internal/config"
)

// ErrInvalidProfile wraps the reason PutProfile refused a profile, one the
// caller must fix rather than retry.
var ErrInvalidProfile = errors.New("invalid profile")

// Profiles returns the stored profiles and the warnings they raise against
// the running upstreams.
func (d *Daemon) Profiles() ([]config.Profile, []string, error) {
	profiles, err := d.store.ListProfiles()
	if err != nil {
		return nil, nil, err
	}
	return profiles, d.config().Warnings(profiles), nil
}

// PutProfile validates p against the running upstreams, creates it or
// replaces the profile of that name, and reconciles, so the peers that use it
// move before it returns. It returns the warnings of every profile.
func (d *Daemon) PutProfile(p config.Profile) ([]string, error) {
	cfg := d.config()
	if err := cfg.ValidateProfile(p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}
	if err := d.store.PutProfile(p); err != nil {
		return nil, err
	}
	// The profile is stored; a peer that cannot be routed now is retried
	// every round, like after `user add`.
	if err := d.Reconcile(); err != nil {
		d.log.Error("reconcile after a profile change", "profile", p.Name, "err", err)
	}
	profiles, err := d.store.ListProfiles()
	if err != nil {
		return nil, err
	}
	return cfg.Warnings(profiles), nil
}

// DeleteProfile removes a profile no peer uses: store.ErrInUse while one
// does, store.ErrNotFound if there is none.
func (d *Daemon) DeleteProfile(name string) error {
	return d.store.DeleteProfile(name)
}

func (d *Daemon) config() *config.Config {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg
}

// checkProfiles validates the stored profiles against cfg's upstreams and
// logs their warnings. Start passes no running config, Reload the one it
// replaces. A reload that takes the last upstream from a profile the running
// config serves is refused, so the gateway keeps its config. Any other profile
// left without an upstream only warns and its peers stay blocked: one stored
// row must not keep the whole gateway down, nor block every later reload.
func (d *Daemon) checkProfiles(cfg, running *config.Config) error {
	profiles, err := d.store.ListProfiles()
	if err != nil {
		return err
	}
	for _, p := range profiles {
		if err := cfg.ValidateProfile(p); err != nil {
			if running != nil && running.ValidateProfile(p) == nil {
				return err
			}
			d.log.Warn("profile has no upstream to use, its peers stay blocked", "err", err)
		}
	}
	for _, w := range cfg.Warnings(profiles) {
		d.log.Warn("profile warning", "detail", w)
	}
	return nil
}
