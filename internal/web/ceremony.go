package web

import (
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

type ceremonyKind int

const (
	signIn ceremonyKind = iota
	enroll
)

// ceremony is a WebAuthn ceremony between its begin and finish requests.
type ceremony struct {
	kind    ceremonyKind
	session webauthn.SessionData
	expires time.Time
	// For enroll: the redeemed token's hash and the operator to be.
	enrollment []byte
	operator   *passkey
}

// ceremonies keeps the open ceremonies in memory, keyed by challenge: the
// finish request carries its challenge inside the signed client data, so
// nothing else ties the two requests together. Each is taken once, so a
// replayed response finds nothing. A restart forgets them; the browser
// starts again.
type ceremonies struct {
	max int

	mu   sync.Mutex
	open map[string]ceremony
}

func newCeremonies(max int) *ceremonies {
	return &ceremonies{max: max, open: make(map[string]ceremony)}
}

// put opens c; false when too many are open.
func (cs *ceremonies) put(c ceremony, now time.Time) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.open) >= cs.max {
		for challenge, c := range cs.open {
			if !now.Before(c.expires) {
				delete(cs.open, challenge)
			}
		}
		if len(cs.open) >= cs.max {
			return false
		}
	}
	cs.open[c.session.Challenge] = c
	return true
}

// take closes the ceremony of challenge and returns it, if it is of kind
// and has not expired.
func (cs *ceremonies) take(challenge string, kind ceremonyKind, now time.Time) (ceremony, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, ok := cs.open[challenge]
	delete(cs.open, challenge)
	return c, ok && c.kind == kind && now.Before(c.expires)
}
