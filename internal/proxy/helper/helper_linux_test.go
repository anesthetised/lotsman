package helper

import (
	"os"
	"syscall"
	"testing"
)

// The production command runs this binary's own image, dies with the
// daemon, and drops a root daemon's privileges.
func TestSelf(t *testing.T) {
	cmd := Self()
	if cmd.Path != "/proc/self/exe" || len(cmd.Args) != 2 || cmd.Args[1] != Subcommand {
		t.Errorf("command = %s %v", cmd.Path, cmd.Args)
	}
	if cmd.Env == nil || len(cmd.Env) != 0 || cmd.Dir != "/" {
		t.Errorf("env %v (nil inherits the daemon's), dir %q", cmd.Env, cmd.Dir)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("SysProcAttr = %+v, want Pdeathsig SIGKILL", cmd.SysProcAttr)
	}
	cred := cmd.SysProcAttr.Credential
	if os.Geteuid() == 0 {
		if cred == nil || cred.Uid != Nobody || cred.Gid != Nobody || len(cred.Groups) != 0 || cred.NoSetGroups {
			t.Errorf("credential = %+v, want uid/gid %d and no groups", cred, Nobody)
		}
	} else if cred != nil {
		t.Errorf("a non-root daemon changes the helper's user: %+v", cred)
	}
}
