package traffic

import (
	"fmt"

	"github.com/vishvananda/netlink"
)

// InterfaceCounters reads an interface's byte counters as the kernel keeps
// them, over netlink, so in the caller's network namespace (sysfs shows the
// namespace it was mounted in). For a TUN, rx is what the program behind it
// wrote into the kernel and tx what the kernel routed into it.
func InterfaceCounters(name string) (rx, tx uint64, err error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return 0, 0, err
	}
	st := link.Attrs().Statistics
	if st == nil {
		return 0, 0, fmt.Errorf("%s: no statistics", name)
	}
	return st.RxBytes, st.TxBytes, nil
}
