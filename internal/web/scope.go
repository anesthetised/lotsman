package web

import (
	"errors"
	"net/http"

	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
)

// Scope is who may call an RPC through the panel.
type Scope int

const (
	// Internal RPCs are the panel's own: a browser never reaches them.
	Internal Scope = iota
	// Public RPCs answer anyone.
	Public
	// Operator RPCs need a signed-in operator.
	Operator
	// User RPCs need a user's link; the daemon gets the user in the
	// Lotsman-User header.
	User
)

func (s Scope) String() string {
	switch s {
	case Internal:
		return "internal"
	case Public:
		return "public"
	case Operator:
		return "operator"
	case User:
		return "user"
	}
	return "unknown"
}

// Scopes holds every RPC of lotsman.v1 that the panel forwards. An RPC
// missing from it is refused; a test fails until a new RPC gets a scope.
var Scopes = map[string]Scope{
	lotsmanv1connect.StatusServiceWatchProcedure: Operator,

	lotsmanv1connect.HistoryServiceUpstreamProcedure: Operator,
	lotsmanv1connect.HistoryServicePeerProcedure:     Operator,

	lotsmanv1connect.ConnectionServiceWatchProcedure: Operator,

	lotsmanv1connect.ProfileServiceListProcedure:   Operator,
	lotsmanv1connect.ProfileServicePutProcedure:    Operator,
	lotsmanv1connect.ProfileServiceDeleteProcedure: Operator,

	lotsmanv1connect.UserServiceListProcedure:         Operator,
	lotsmanv1connect.UserServiceAddDeviceProcedure:    Operator,
	lotsmanv1connect.UserServiceRemoveDeviceProcedure: Operator,
	lotsmanv1connect.UserServiceRemoveUserProcedure:   Operator,
	lotsmanv1connect.UserServiceGetConfigProcedure:    Operator,
	lotsmanv1connect.UserServiceIssueLinkProcedure:    Operator,
	lotsmanv1connect.UserServiceRevokeLinkProcedure:   Operator,

	// The panel resolves a link itself before it sets Lotsman-User.
	lotsmanv1connect.UserSelfServiceResolveLinkProcedure:     Internal,
	lotsmanv1connect.UserSelfServiceGetProcedure:             User,
	lotsmanv1connect.UserSelfServiceWatchProcedure:           User,
	lotsmanv1connect.UserSelfServiceAddDeviceProcedure:       User,
	lotsmanv1connect.UserSelfServiceRemoveDeviceProcedure:    User,
	lotsmanv1connect.UserSelfServiceGetConfigProcedure:       User,
	lotsmanv1connect.UserSelfServiceListProfilesProcedure:    User,
	lotsmanv1connect.UserSelfServiceUpstreamHistoryProcedure: User,
	lotsmanv1connect.UserSelfServicePeerHistoryProcedure:     User,
}

// Authenticator tells who sent a request. Operator sessions (#133) and user
// links (#134) implement it; until then NoAuth refuses everyone.
type Authenticator interface {
	// Operator returns nil when r comes from a signed-in operator.
	Operator(r *http.Request) error
	// User returns the user whose link r carries.
	User(r *http.Request) (string, error)
}

// NoAuth authenticates nobody, so only public RPCs answer.
type NoAuth struct{}

var errNoAuth = errors.New("sign-in is not available yet")

func (NoAuth) Operator(*http.Request) error       { return errNoAuth }
func (NoAuth) User(*http.Request) (string, error) { return "", errNoAuth }
