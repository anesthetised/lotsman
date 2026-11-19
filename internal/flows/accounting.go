package flows

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// AccountingSysctl turns on conntrack's per-flow byte counters. It exists
// once the nf_conntrack module is loaded, which the daemon's nftables rules
// do. It is per network namespace, and only flows created after it is set
// are counted.
const AccountingSysctl = "/proc/sys/net/netfilter/nf_conntrack_acct"

// EnableAccounting sets AccountingSysctl to 1 unless it already is, so a
// container with a read-only /proc/sys works when the operator presets it.
// The previous value is not restored on exit: uninstall does that from the
// install manifest, as for net.ipv4.ip_forward.
func EnableAccounting() error {
	return enableAccounting(AccountingSysctl)
}

func enableAccounting(path string) error {
	current, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("conntrack accounting unavailable: no nf_conntrack module")
	}
	if err == nil && strings.TrimSpace(string(current)) == "1" {
		return nil
	}
	if err := os.WriteFile(path, []byte("1"), 0); err != nil {
		return fmt.Errorf("enable conntrack accounting: %w", err)
	}
	return nil
}
