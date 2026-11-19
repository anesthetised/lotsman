package control

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/store"
)

// ProfileSource is what ProfileService needs from the daemon. The daemon
// validates and stores; the service only translates.
type ProfileSource interface {
	Profiles() ([]config.Profile, []string, error)
	// Upstreams are the running upstreams, whose labels tiers match.
	Upstreams() []config.Upstream
	PutProfile(config.Profile) ([]string, error)
	DeleteProfile(name string) error
}

type profileService struct{ src ProfileSource }

func (s profileService) List(context.Context, *lotsmanv1.ListProfilesRequest) (*lotsmanv1.ListProfilesResponse, error) {
	profiles, warnings, err := s.src.Profiles()
	if err != nil {
		return nil, profileError("", err)
	}
	res := &lotsmanv1.ListProfilesResponse{Warnings: warnings}
	for _, p := range profiles {
		res.Profiles = append(res.Profiles, profileMessage(p))
	}
	for _, u := range s.src.Upstreams() {
		res.Upstreams = append(res.Upstreams, &lotsmanv1.UpstreamLabels{Name: u.Name, Geo: u.Geo, Provider: u.Provider, Tags: u.Tags})
	}
	return res, nil
}

// Put creates the profile or replaces the one of its name; the peers that
// use it move before it answers.
func (s profileService) Put(_ context.Context, req *lotsmanv1.PutProfileRequest) (*lotsmanv1.PutProfileResponse, error) {
	if req.GetProfile().GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "profile: name is required")
	}
	warnings, err := s.src.PutProfile(profileConfig(req.GetProfile()))
	if err != nil {
		return nil, profileError(req.GetProfile().GetName(), err)
	}
	return &lotsmanv1.PutProfileResponse{Warnings: warnings}, nil
}

// Delete refuses a profile that peers still use.
func (s profileService) Delete(_ context.Context, req *lotsmanv1.DeleteProfileRequest) (*lotsmanv1.DeleteProfileResponse, error) {
	if err := s.src.DeleteProfile(req.GetName()); err != nil {
		return nil, profileError(req.GetName(), err)
	}
	return &lotsmanv1.DeleteProfileResponse{}, nil
}

// profileError maps the daemon's errors to codes. The reasons the caller
// can act on are passed on; anything else is the daemon's own failure and
// stays inside it.
func profileError(name string, err error) error {
	switch {
	case errors.Is(err, daemon.ErrInvalidProfile):
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	case errors.Is(err, store.ErrInUse):
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("profile %q is in use: remove its peers first", name))
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, fmt.Sprintf("profile %q not found", name))
	}
	return connect.NewError(connect.CodeInternal, "profiles: internal error")
}

func profileMessage(p config.Profile) *lotsmanv1.Profile {
	m := &lotsmanv1.Profile{Name: p.Name}
	for _, t := range p.Prefer {
		m.Prefer = append(m.Prefer, &lotsmanv1.Matcher{Geo: t.Geo, Provider: t.Provider, Tag: t.Tag})
	}
	return m
}

func profileConfig(m *lotsmanv1.Profile) config.Profile {
	p := config.Profile{Name: m.GetName()}
	for _, t := range m.GetPrefer() {
		p.Prefer = append(p.Prefer, config.Match{Geo: t.GetGeo(), Provider: t.GetProvider(), Tag: t.GetTag()})
	}
	return p
}
