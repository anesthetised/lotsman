package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/daemon"
)

// fakeSource is a daemon whose rounds the test finishes by hand.
type fakeSource struct {
	ready  chan struct{}
	rounds chan struct{}

	mu sync.Mutex
	st daemon.Status
}

func (f *fakeSource) Ready() <-chan struct{}  { return f.ready }
func (f *fakeSource) Rounds() <-chan struct{} { return f.rounds }
func (f *fakeSource) ClientMTU() int          { return 1280 }

func (f *fakeSource) Status() daemon.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeSource) setRouted(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st = daemon.Status{Upstreams: []daemon.UpstreamStatus{{Name: "nl", Routed: n}}}
}

// The status file follows the daemon's rounds, so it needs no interval of
// its own and cannot keep the one it started with after a reload.
func TestPublishStatusFollowsRounds(t *testing.T) {
	dir := t.TempDir()
	src := &fakeSource{ready: make(chan struct{}), rounds: make(chan struct{}, 1)}
	src.setRouted(1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		publishStatus(ctx, src, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	// Nothing is published before the daemon is ready.
	time.Sleep(20 * time.Millisecond)
	if _, err := readStatus(dir); err == nil {
		t.Fatal("status published before ready")
	}
	close(src.ready)
	eventuallyStatus(t, dir, "initial snapshot", 1)

	// A later round, however much time has passed, is published at once.
	src.setRouted(2)
	src.rounds <- struct{}{}
	eventuallyStatus(t, dir, "snapshot after a round", 2)

	cancel()
	<-done
	if _, err := readStatus(dir); err == nil {
		t.Error("status file left behind after shutdown")
	}
}

func eventuallyStatus(t *testing.T, dir, what string, routed int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := readStatus(dir); err == nil && len(s.Upstreams) == 1 && s.Upstreams[0].Routed == routed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (routed %d)", what, routed)
}
