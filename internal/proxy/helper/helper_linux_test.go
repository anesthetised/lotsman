package helper

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"os/user"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// fakeLookup is an os/user.Lookup with one user at most.
func fakeLookup(u *user.User, err error) func(string) (*user.User, error) {
	return func(name string) (*user.User, error) {
		if name != ProxyUser {
			return nil, user.UnknownUserError(name)
		}
		return u, err
	}
}

func newResolver(lookup func(string) (*user.User, error)) (*credResolver, *bytes.Buffer) {
	var buf bytes.Buffer
	return &credResolver{lookup: lookup, log: slog.New(slog.NewTextHandler(&buf, nil))}, &buf
}

// The production command runs this binary's own image, dies with the
// daemon, and drops a root daemon's privileges.
func TestSelf(t *testing.T) {
	r, _ := newResolver(fakeLookup(nil, user.UnknownUserError(ProxyUser)))
	t.Setenv("SSL_CERT_FILE", "")
	os.Unsetenv("SSL_CERT_FILE")
	t.Setenv("SSL_CERT_DIR", "/etc/lotsman/ca")
	t.Setenv("LOTSMAN_TEST_SECRET", "x")
	cmd := command(r)()
	if cmd.Path != "/proc/self/exe" || len(cmd.Args) != 2 || cmd.Args[1] != Subcommand {
		t.Errorf("command = %s %v", cmd.Path, cmd.Args)
	}
	if !slices.Equal(cmd.Env, []string{"SSL_CERT_DIR=/etc/lotsman/ca"}) || cmd.Dir != "/" {
		t.Errorf("env %v (want only the TLS roots), dir %q", cmd.Env, cmd.Dir)
	}
	os.Unsetenv("SSL_CERT_DIR")
	if env := command(r)().Env; env == nil || len(env) != 0 {
		t.Errorf("env %v (nil inherits the daemon's)", env)
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

// A root daemon runs helpers as the dedicated user's uid and its primary
// group, with no supplementary groups; no warning when it exists.
func TestCredentialDedicatedUser(t *testing.T) {
	r, log := newResolver(fakeLookup(&user.User{Uid: "998", Gid: "997"}, nil))
	c := r.credential()
	if c.Uid != 998 || c.Gid != 997 || len(c.Groups) != 0 || c.NoSetGroups {
		t.Errorf("credential = %+v, want uid 998, gid 997, no groups", c)
	}
	if log.Len() != 0 {
		t.Errorf("logged for a present user: %s", log)
	}
}

// Without the user, or with one that must not run a helper, the helper
// falls back to nobody and the daemon says so once however often helpers
// start.
func TestCredentialFallback(t *testing.T) {
	for name, lookup := range map[string]func(string) (*user.User, error){
		"missing":     fakeLookup(nil, user.UnknownUserError(ProxyUser)),
		"lookup fail": fakeLookup(nil, errors.New("nss down")),
		"root uid":    fakeLookup(&user.User{Uid: "0", Gid: "997"}, nil),
		"root gid":    fakeLookup(&user.User{Uid: "998", Gid: "0"}, nil),
		"bad uid":     fakeLookup(&user.User{Uid: "x", Gid: "997"}, nil),
		"bad gid":     fakeLookup(&user.User{Uid: "998", Gid: ""}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			r, log := newResolver(lookup)
			for range 3 {
				c := r.credential()
				if c.Uid != Nobody || c.Gid != Nobody || len(c.Groups) != 0 || c.NoSetGroups {
					t.Fatalf("credential = %+v, want nobody and no groups", c)
				}
			}
			if n := strings.Count(log.String(), "level=WARN"); n != 1 || !strings.Contains(log.String(), ProxyUser) {
				t.Errorf("want one warning naming %s, got %d:\n%s", ProxyUser, n, log)
			}
		})
	}
}

// The user is looked up at every start: creating it later takes effect
// without a daemon restart, and the fallback warning is not repeated.
func TestCredentialUserAppears(t *testing.T) {
	var present *user.User
	r, log := newResolver(func(name string) (*user.User, error) {
		if present == nil {
			return nil, user.UnknownUserError(name)
		}
		return present, nil
	})
	if c := r.credential(); c.Uid != Nobody {
		t.Fatalf("before: %+v", c)
	}
	present = &user.User{Uid: "998", Gid: "997"}
	if c := r.credential(); c.Uid != 998 || c.Gid != 997 {
		t.Errorf("after: %+v", c)
	}
	if n := strings.Count(log.String(), "level=WARN"); n != 1 {
		t.Errorf("warnings = %d, want 1", n)
	}
}
