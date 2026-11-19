package control

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/store"
)

// fakeProfiles answers with what the test sets and records what it is given.
type fakeProfiles struct {
	profiles []config.Profile
	warnings []string
	err      error

	put     []config.Profile
	deleted []string
}

func (f *fakeProfiles) Profiles() ([]config.Profile, []string, error) {
	return f.profiles, f.warnings, f.err
}

func (f *fakeProfiles) PutProfile(p config.Profile) ([]string, error) {
	f.put = append(f.put, p)
	return f.warnings, f.err
}

func (f *fakeProfiles) DeleteProfile(name string) error {
	f.deleted = append(f.deleted, name)
	return f.err
}

func profileClient(src ProfileSource) lotsmanv1connect.ProfileServiceClient {
	s := connect.NewServer()
	lotsmanv1connect.RegisterProfileServiceHandler(s, profileService{src})
	return lotsmanv1connect.NewProfileServiceClient(connect.NewClient(connectinprocess.New(s)))
}

var (
	euProfile = config.Profile{Name: "eu", Prefer: []config.Match{{Geo: "NL", Provider: "acme"}, {Tag: "eu"}, {}}}
	euMessage = &lotsmanv1.Profile{Name: "eu", Prefer: []*lotsmanv1.Matcher{{Geo: "NL", Provider: "acme"}, {Tag: "eu"}, {}}}
)

func TestProfileList(t *testing.T) {
	src := &fakeProfiles{
		profiles: []config.Profile{euProfile, {Name: "any"}},
		warnings: []string{`profile "eu": tier 3 matches nothing new`},
	}
	got, err := profileClient(src).List(t.Context(), &lotsmanv1.ListProfilesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Tiers keep their order; an empty tier and an empty prefer stay empty.
	want := &lotsmanv1.ListProfilesResponse{
		Profiles: []*lotsmanv1.Profile{euMessage, {Name: "any"}},
		Warnings: src.warnings,
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("list (-want +got):\n%s", diff)
	}
}

func TestProfilePut(t *testing.T) {
	src := &fakeProfiles{warnings: []string{"a warning"}}
	got, err := profileClient(src).Put(t.Context(), &lotsmanv1.PutProfileRequest{Profile: euMessage})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]config.Profile{euProfile}, src.put); diff != "" {
		t.Errorf("stored (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"a warning"}, got.Warnings); diff != "" {
		t.Errorf("warnings (-want +got):\n%s", diff)
	}
}

func TestProfilePutNeedsName(t *testing.T) {
	src := &fakeProfiles{}
	client := profileClient(src)
	for _, req := range []*lotsmanv1.PutProfileRequest{{}, {Profile: &lotsmanv1.Profile{Prefer: euMessage.Prefer}}} {
		if _, err := client.Put(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("put %v: %v, want invalid_argument", req, err)
		}
	}
	if len(src.put) != 0 {
		t.Errorf("stored %v", src.put)
	}
}

func TestProfileDelete(t *testing.T) {
	src := &fakeProfiles{}
	if _, err := profileClient(src).Delete(t.Context(), &lotsmanv1.DeleteProfileRequest{Name: "eu"}); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"eu"}, src.deleted); diff != "" {
		t.Errorf("deleted (-want +got):\n%s", diff)
	}
}

// The daemon's errors become codes the panel can act on; one it did not
// expect says nothing of the daemon's internals.
func TestProfileErrors(t *testing.T) {
	internal := errors.New("database is locked at /var/lib/lotsman/lotsman.db")
	for _, tc := range []struct {
		err  error
		code connect.Code
		says string // the reason the caller can act on
	}{
		{fmt.Errorf("%w: prefer matches no upstream", daemon.ErrInvalidProfile), connect.CodeInvalidArgument, "prefer matches no upstream"},
		{store.ErrInUse, connect.CodeFailedPrecondition, `profile "eu" is in use`},
		{store.ErrNotFound, connect.CodeNotFound, `profile "eu" not found`},
		{internal, connect.CodeInternal, ""},
	} {
		client := profileClient(&fakeProfiles{err: tc.err})
		calls := map[string]func() error{
			"list": func() error {
				_, err := client.List(t.Context(), &lotsmanv1.ListProfilesRequest{})
				return err
			},
			"put": func() error {
				_, err := client.Put(t.Context(), &lotsmanv1.PutProfileRequest{Profile: euMessage})
				return err
			},
			"delete": func() error {
				_, err := client.Delete(t.Context(), &lotsmanv1.DeleteProfileRequest{Name: "eu"})
				return err
			},
		}
		for name, call := range calls {
			err := call()
			if connect.CodeOf(err) != tc.code {
				t.Errorf("%s with %q: %v, want %v", name, tc.err, err, tc.code)
			}
			// list names no profile.
			if msg := fmt.Sprint(err); name != "list" && !strings.Contains(msg, tc.says) || strings.Contains(msg, "lotsman.db") {
				t.Errorf("%s: message %q, want %q and no internals", name, msg, tc.says)
			}
		}
	}
}
