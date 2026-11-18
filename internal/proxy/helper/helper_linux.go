package helper

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"golang.org/x/sys/unix"
)

// ProxyUser is the system user, and its primary group, a helper runs as when
// the daemon is root and the install created it.
const ProxyUser = "lotsman-proxy"

// Nobody is the user and group a helper falls back to when ProxyUser does
// not exist.
const Nobody = 65534

// credResolver picks the helper's credentials. lookup is os/user.Lookup in
// production. The fallback is logged once, not at every helper start, so a
// restarting helper does not flood the log.
type credResolver struct {
	lookup func(name string) (*user.User, error)
	log    *slog.Logger
	warned sync.Once
}

// credential is ProxyUser's uid and primary gid with no supplementary
// groups, or nobody's with a warning. The user is looked up at every start,
// so creating it takes effect when a helper next starts, with no daemon
// restart. A user or group that resolves to root is refused: a misconfigured
// account must never run a helper as root.
func (r *credResolver) credential() *syscall.Credential {
	u, err := r.lookup(ProxyUser)
	if err == nil {
		var c *syscall.Credential
		if c, err = credentialOf(u); err == nil {
			return c
		}
	}
	r.warned.Do(func() {
		r.log.Warn("proxy helpers run as uid/gid 65534 (nobody), which other services may share; "+
			"create the dedicated user, see the operations guide",
			"user", ProxyUser, "error", err)
	})
	return &syscall.Credential{Uid: Nobody, Gid: Nobody}
}

func credentialOf(u *user.User) (*syscall.Credential, error) {
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("gid %q: %w", u.Gid, err)
	}
	if uid == 0 || gid == 0 {
		return nil, errors.New("uid or gid 0: refusing to run a helper as root")
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}

// Self returns the production Runner.Command: this binary's helper
// subcommand. log receives the warning when the helpers cannot use ProxyUser.
//
// It runs /proc/self/exe, so a binary replaced on disk by an upgrade still
// starts the daemon's own version, which speaks its protocol. The kernel
// kills the helper if the daemon dies. A root daemon drops the helper to
// ProxyUser's uid and gid, or 65534 if that user does not exist, with no
// supplementary groups; with no file capabilities on the binary, exec then
// leaves it no capabilities at all. It keeps what it inherits: the TUN
// descriptor, and outbound sockets need no privilege. The working directory
// is /, and the environment is empty but for where TLS roots come from
// (SSL_CERT_FILE, SSL_CERT_DIR), if the daemon's sets that: a helper verifies
// wss:// servers against the roots the daemon verifies ssconf:// servers
// against. Such a file must be readable by the helper's user.
func Self(log *slog.Logger) func() *exec.Cmd {
	return command(&credResolver{lookup: user.Lookup, log: log})
}

func command(creds *credResolver) func() *exec.Cmd {
	return func() *exec.Cmd {
		cmd := exec.Command("/proc/self/exe", Subcommand)
		cmd.Args[0] = "lotsman"
		cmd.Env = []string{}
		for _, name := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
			if v, ok := os.LookupEnv(name); ok {
				cmd.Env = append(cmd.Env, name+"="+v)
			}
		}
		cmd.Dir = "/"
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if os.Geteuid() == 0 {
			cmd.SysProcAttr.Credential = creds.credential()
		}
		return cmd
	}
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
