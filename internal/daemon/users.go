package daemon

import (
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/policy"
	"github.com/anesthetised/lotsman/internal/store"
)

// The control API's user services change users and peers through these
// methods: the same store calls as the user and device commands, applied at
// once instead of after the CLI's SIGUSR1.

// Store is the database the daemon applies. The control API reads users
// and peers from it and keeps user links there.
func (d *Daemon) Store() *store.Store { return d.store }

// Upstreams returns the running config's upstreams, which routes and exits
// are validated against.
func (d *Daemon) Upstreams() []config.Upstream { return d.config().Upstreams }

// AddPeer stores a new device with its route in the gateway's subnet and
// applies it, so the client can connect as soon as it returns. With
// createUser a missing user is created, as `device add` does; without, it
// is store.ErrNotFound. The caller validates the route.
func (d *Daemon) AddPeer(user, device string, route policy.Route, createUser bool) (store.Peer, error) {
	add := d.store.AddPeer
	if createUser {
		add = d.store.AddUserPeer
	}
	p, err := add(user, device, route, d.config().Subnet)
	if err != nil {
		return store.Peer{}, err
	}
	d.apply("adding a peer")
	return p, nil
}

// SetRoute replaces a device's route and applies it, so the device has moved
// when it returns. The caller validates the route.
func (d *Daemon) SetRoute(user, device string, route policy.Route) (store.Peer, error) {
	p, err := d.store.SetRoute(user, device, route)
	if err != nil {
		return store.Peer{}, err
	}
	d.apply("changing a route")
	return p, nil
}

// SetRights replaces a user's rights and applies them, so their devices
// have left exits no longer allowed when it returns. The caller validates
// the exits.
func (d *Daemon) SetRights(user string, r store.Rights) (store.User, error) {
	u, err := d.store.SetRights(user, r)
	if err != nil {
		return store.User{}, err
	}
	d.apply("changing rights")
	return u, nil
}

// RemovePeer removes one of the user's devices and applies it.
func (d *Daemon) RemovePeer(user, device string) error {
	if err := d.store.DeletePeer(user, device); err != nil {
		return err
	}
	d.apply("removing a peer")
	return nil
}

// RemoveUser removes the user, their peers and their link, and applies it.
func (d *Daemon) RemoveUser(name string) error {
	if err := d.store.DeleteUser(name); err != nil {
		return err
	}
	d.apply("removing a user")
	return nil
}

// PeerConfig is the client config of p, as `user show` prints it.
func (d *Daemon) PeerConfig(p store.Peer) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return ClientConfig(d.cfg, d.id, p, d.clientMTU()).String()
}

// MaxDevices is users.max_devices of the running config.
func (d *Daemon) MaxDevices() int { return d.config().Users.MaxDevices }

func (d *Daemon) config() *config.Config {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg
}

// apply reconciles after a change to the store. Before the first round
// there is no device to change yet: the round is woken instead, so it
// cannot miss a change made after it read the store.
func (d *Daemon) apply(what string) {
	select {
	case <-d.ready:
	default:
		select {
		case d.wake <- struct{}{}:
		default:
		}
		return
	}
	// The change is stored; what cannot be applied now is retried every
	// round, as after `user add`.
	if err := d.Reconcile(); err != nil {
		d.log.Error("reconcile after "+what, "err", err)
	}
}
