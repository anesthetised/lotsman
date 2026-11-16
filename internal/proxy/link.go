package proxy

import (
	"fmt"
	"log/slog"
	"net/netip"
	"sync"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

// Spec is what a proxy upstream's stack runs with.
type Spec struct {
	Key      string         `json:"key"`       // the access key as ParseConfig reads it: a static key or a dynamic key's response; a secret
	Server   netip.AddrPort `json:"server"`    // the key's server, resolved
	MaxFlows int            `json:"max_flows"` // Options.MaxFlows
}

// Link is a proxy upstream's stack, running in this process or in a helper
// process. Its methods are safe for concurrent use.
type Link interface {
	// Up makes the link carry traffic. For a helper it starts the process,
	// and starts it again after it exited.
	Up() error
	// SetServer points new flows at the server's current address and key,
	// the key's text as in Spec; a key that did not change is not parsed
	// again.
	SetServer(key string, addr netip.AddrPort) error
	// SetMaxFlows changes Options.MaxFlows; 0 means the default.
	SetMaxFlows(int) error
	Stats() (Stats, error)
	// Close stops the stack and closes the TUN.
	Close()
}

// Runner starts a Link for spec on t, which it owns from then on, also when
// it fails. exited is called when a link that is up stops on its own, such
// as when its helper process dies; Up starts it again. It may be called
// concurrently with, or just after, Close.
type Runner func(t tun.Device, spec Spec, log *slog.Logger, exited func(error)) (Link, error)

// InProcess runs the stack in this process, carrying traffic at once: a
// panic in it takes the whole process down. It calls exited, if set, when
// the stack stops because reading the TUN failed; Up cannot start it again.
// A helper process exits then, so its runner restarts it.
func InProcess(t tun.Device, spec Spec, log *slog.Logger, exited func(error)) (Link, error) {
	key, err := ParseConfig(spec.Key)
	if err != nil {
		t.Close()
		return nil, err
	}
	d, err := key.Dialers(spec.Server)
	if err != nil {
		t.Close()
		return nil, err
	}
	// New owns t and d from here on, also when it fails.
	s, err := New(t, d, Options{MaxFlows: spec.MaxFlows}, log)
	if err != nil {
		return nil, err
	}
	if exited != nil {
		go func() {
			if err, failed := <-s.Failed(); failed {
				exited(fmt.Errorf("proxy stack stopped: %w", err))
			}
		}()
	}
	return &localLink{stack: s, key: key, text: spec.Key}, nil
}

type localLink struct {
	stack *Stack

	mu   sync.Mutex
	key  Key
	text string // what key was parsed from
}

func (l *localLink) Up() error { return nil }
func (l *localLink) Close()    { l.stack.Close() }

// SetServer swaps the dialers at once: new flows reach the new address with
// the new key, open ones stay where they are. The key is parsed again only
// when its text changed.
func (l *localLink) SetServer(key string, addr netip.AddrPort) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := l.key
	if key != l.text {
		var err error
		if k, err = ParseConfig(key); err != nil {
			return err
		}
	}
	d, err := k.Dialers(addr)
	if err != nil {
		return err
	}
	l.stack.SetDialers(d)
	l.key, l.text = k, key
	return nil
}

func (l *localLink) SetMaxFlows(n int) error {
	l.stack.SetMaxFlows(n)
	return nil
}

func (l *localLink) Stats() (Stats, error) { return l.stack.Stats(), nil }
