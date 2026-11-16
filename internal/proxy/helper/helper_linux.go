package helper

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"golang.org/x/sys/unix"
)

// Nobody is the user and group a helper runs as when the daemon is root.
const Nobody = 65534

// Self is the production Runner.Command: this binary's helper subcommand.
//
// It runs /proc/self/exe, so a binary replaced on disk by an upgrade still
// starts the daemon's own version, which speaks its protocol. The kernel
// kills the helper if the daemon dies. A root daemon drops the helper to
// uid and gid 65534 with no supplementary groups; with no file
// capabilities on the binary, exec then leaves it no capabilities at all.
// It keeps what it inherits: the TUN descriptor, and outbound sockets need
// no privilege. The environment is empty and the working directory is /.
func Self() *exec.Cmd {
	cmd := exec.Command("/proc/self/exe", Subcommand)
	cmd.Args[0] = "lotsman"
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: Nobody, Gid: Nobody}
	}
	return cmd
}

// openTUN takes over the daemon's TUN. The daemon created and configured
// it; the helper only reads and writes packets, so nothing here needs a
// capability.
func openTUN() (tun.Device, error) {
	t, _, err := tun.CreateUnmonitoredTUNFromFD(tunFD)
	return t, err
}

// harden keeps processes of the same unprivileged user from attaching to
// the helper or reading its memory, which holds the access key: a
// non-dumpable process's /proc entries belong to root.
func harden() error {
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}
