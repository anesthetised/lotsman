package control

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/internal/history"
	"github.com/anesthetised/lotsman/internal/store"
)

// HistorySource is what HistoryService reads: the daemon, or a fake in
// tests. The same queries serve the user-scoped history RPCs.
type HistorySource interface {
	UpstreamHistory(name string, r history.Range) (history.Result, error)
	PeerHistory(id int64, r history.Range) (history.Result, error)
}

type historyService struct{ src HistorySource }

func (s historyService) Upstream(_ context.Context, req *lotsmanv1.HistoryRequest) (*lotsmanv1.HistoryResponse, error) {
	if req.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name is required")
	}
	r, err := historyRange(req)
	if err != nil {
		return nil, err
	}
	res, err := s.src.UpstreamHistory(req.GetName(), r)
	return historyResponse(res, err)
}

func (s historyService) Peer(_ context.Context, req *lotsmanv1.HistoryRequest) (*lotsmanv1.HistoryResponse, error) {
	if req.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name is required")
	}
	r, err := historyRange(req)
	if err != nil {
		return nil, err
	}
	id, ok := ParsePeerID(req.GetName())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "no such peer")
	}
	res, err := s.src.PeerHistory(id, r)
	return historyResponse(res, err)
}

// ParsePeerID is PeerID's inverse; ok is false for anything PeerID does not
// produce.
func ParsePeerID(name string) (id int64, ok bool) {
	digits, found := strings.CutPrefix(name, "p_")
	if !found || digits == "" || digits[0] == '+' || digits[0] == '-' {
		return 0, false
	}
	id, err := strconv.ParseInt(digits, 10, 64)
	return id, err == nil && PeerID(id) == name
}

// historyRange reads the request's timestamps and step; unset ones stay
// zero for history.Resolve to default or refuse.
func historyRange(req *lotsmanv1.HistoryRequest) (history.Range, error) {
	var r history.Range
	if t := req.GetFrom(); t != nil {
		if err := t.CheckValid(); err != nil {
			return r, connect.NewError(connect.CodeInvalidArgument, "from: "+err.Error())
		}
		r.From = t.AsTime()
	}
	if t := req.GetTo(); t != nil {
		if err := t.CheckValid(); err != nil {
			return r, connect.NewError(connect.CodeInvalidArgument, "to: "+err.Error())
		}
		r.To = t.AsTime()
	}
	if d := req.GetStep(); d != nil {
		if err := d.CheckValid(); err != nil {
			return r, connect.NewError(connect.CodeInvalidArgument, "step: "+err.Error())
		}
		r.Step = d.AsDuration()
	}
	return r, nil
}

func historyResponse(res history.Result, err error) (*lotsmanv1.HistoryResponse, error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, connect.NewError(connect.CodeNotFound, "no such upstream or peer")
	case errors.Is(err, history.ErrRange):
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, "history is unavailable")
	}
	out := &lotsmanv1.HistoryResponse{Step: duration(res.Step), Samples: make([]*lotsmanv1.Sample, len(res.Samples))}
	for i, s := range res.Samples {
		out.Samples[i] = &lotsmanv1.Sample{
			Time: timestamppb.New(s.Time), UpRatio: float32(s.UpRatio), Latency: duration(s.Latency),
			DownBytes: s.Down, UpBytes: s.Up, DownRate: s.DownRate, UpRate: s.UpRate,
		}
	}
	return out, nil
}
