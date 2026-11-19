package web

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "github.com/anesthetised/lotsman/api/lotsman/v1"
)

// declared lists every RPC of lotsman.v1 from the descriptors, so a new RPC
// shows up here without touching the tests.
func declared() []string {
	var procs []string
	protoregistry.GlobalFiles.RangeFilesByPackage("lotsman.v1", func(f protoreflect.FileDescriptor) bool {
		for i := range f.Services().Len() {
			svc := f.Services().Get(i)
			for j := range svc.Methods().Len() {
				procs = append(procs, "/"+string(svc.FullName())+"/"+string(svc.Methods().Get(j).Name()))
			}
		}
		return true
	})
	slices.Sort(procs)
	return procs
}

// Every RPC has a scope, and the table names no RPC that does not exist.
func TestScopesCoverEveryRPC(t *testing.T) {
	procs := declared()
	if len(procs) == 0 {
		t.Fatal("no lotsman.v1 RPCs registered")
	}
	var scoped []string
	for p := range Scopes {
		scoped = append(scoped, p)
	}
	slices.Sort(scoped)
	if diff := cmp.Diff(procs, scoped); diff != "" {
		t.Errorf("scoped RPCs (-declared +scoped):\n%s", diff)
	}
}

// The scopes the API's comments promise: self-service is the user's, the
// link lookup is the panel's own, and everything else is the operator's. A
// new operator RPC scoped for users would hand them the gateway.
func TestScopesFollowServices(t *testing.T) {
	for p, s := range Scopes {
		want := Operator
		switch {
		case p == "/lotsman.v1.UserSelfService/ResolveLink":
			want = Internal
		case strings.HasPrefix(p, "/lotsman.v1.UserSelfService/"):
			want = User
		}
		if s != want {
			t.Errorf("%s: scope %v, want %v", p, s, want)
		}
	}
}
