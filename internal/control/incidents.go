package control

import (
	"context"
	"strconv"

	"connectrpc.com/connect/v2"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/internal/store"
)

// DefaultIncidents is how many incidents List returns when the request
// sets no limit.
const DefaultIncidents = 200

// IncidentSource is what IncidentService reads: the daemon, or a fake in
// tests.
type IncidentSource interface {
	IncidentsPage(upstream string, limit, offset int) ([]store.Incident, error)
}

type incidentService struct{ src IncidentSource }

func (s incidentService) List(_ context.Context, req *lotsmanv1.ListIncidentsRequest) (*lotsmanv1.ListIncidentsResponse, error) {
	limit := int(req.GetLimit())
	if limit == 0 {
		limit = DefaultIncidents
	}
	incidents, err := s.src.IncidentsPage(req.GetUpstream(), limit+1, int(req.GetOffset()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "incidents are unavailable")
	}
	out := &lotsmanv1.ListIncidentsResponse{HasMore: len(incidents) > limit}
	if out.HasMore {
		incidents = incidents[:limit]
	}
	for _, in := range incidents {
		out.Incidents = append(out.Incidents, &lotsmanv1.Incident{
			Id: IncidentID(in.ID), Upstream: in.Upstream, Kind: incidentKind(in.Kind),
			Started: timestamp(in.Started), Ended: timestamp(in.Ended), Detail: in.Detail, Devices: uint32(in.Devices),
		})
	}
	return out, nil
}

// IncidentID is an incident's API identifier, opaque like PeerID.
func IncidentID(id int64) string { return "i_" + strconv.FormatInt(id, 10) }

func incidentKind(kind store.IncidentKind) lotsmanv1.IncidentKind {
	switch kind {
	case store.IncidentDown:
		return lotsmanv1.IncidentKind_INCIDENT_KIND_DOWN
	case store.IncidentDownload:
		return lotsmanv1.IncidentKind_INCIDENT_KIND_DOWNLOAD
	case store.IncidentUpload:
		return lotsmanv1.IncidentKind_INCIDENT_KIND_UPLOAD
	case store.IncidentSendErrors:
		return lotsmanv1.IncidentKind_INCIDENT_KIND_SEND_ERRORS
	}
	return lotsmanv1.IncidentKind_INCIDENT_KIND_UNSPECIFIED
}
