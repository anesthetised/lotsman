package daemon

import "github.com/anesthetised/lotsman/internal/store"

// The control API's user services change users and peers through these
// methods: the same store calls as the user commands, applied at once
// instead of after the CLI's SIGUSR1.

// Store is the database the daemon applies. The control API reads users,
// peers and profiles from it and keeps user links there.
func (d *Daemon) Store() *store.Store { return d.store }

// AddPeer stores a new peer in the gateway's subnet and applies it, so the
// client can connect as soon as it returns. With createUser a missing user
// is created, as `user add` does; without, it is store.ErrNotFound.
func (d *Daemon) AddPeer(user, device, profile string, createUser bool) (store.Peer, error) {
	add := d.store.AddPeer
	if createUser {
		add = d.store.AddUserPeer
	}
	p, err := add(user, device, profile, d.config().Subnet)
	if err != nil {
		return store.Peer{}, err
	}
	d.apply("adding a peer")
	return p, nil
}

// RemovePeers removes the device's peer of that profile, or every peer of
// the device when profile is "", and applies it.
func (d *Daemon) RemovePeers(user, device, profile string) error {
	var err error
	if profile == "" {
		err = d.store.DeleteDevice(user, device)
	} else {
		err = d.store.DeletePeer(user, device, profile)
	}
	if err != nil {
		return err
	}
	d.apply("removing peers")
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
