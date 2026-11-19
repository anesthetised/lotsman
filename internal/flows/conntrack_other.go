//go:build !linux

package flows

import "errors"

// Conntrack reads the conntrack table, which exists on Linux only.
type Conntrack struct{}

func (Conntrack) Entries() ([]Entry, error) {
	return nil, errors.New("conntrack needs Linux")
}
