//go:build !linux

package flows

import (
	"errors"
	"fmt"
)

// Conntrack reads the conntrack table, which exists on Linux only.
type Conntrack struct{}

// Entries fails with errors.ErrUnsupported, so callers can tell a platform
// without conntrack from a read that failed.
func (Conntrack) Entries() ([]Entry, error) {
	return nil, fmt.Errorf("conntrack needs Linux: %w", errors.ErrUnsupported)
}
