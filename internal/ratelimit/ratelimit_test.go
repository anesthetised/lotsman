package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	l := New[string](time.Minute, 3)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		if !l.Allow("alice", now) {
			t.Fatalf("event %d of the burst refused", i+1)
		}
	}
	if !l.Exhausted("alice", now) || l.Allow("alice", now) {
		t.Fatal("event past the burst allowed")
	}
	if l.Exhausted("bob", now) || !l.Allow("bob", now) {
		t.Error("alice's limit applied to bob")
	}
	// Exhausted takes nothing: a check is not an event.
	for range 10 {
		l.Exhausted("carol", now)
	}
	if !l.Allow("carol", now) {
		t.Error("checks used up carol's tokens")
	}

	if l.Allow("alice", now.Add(59*time.Second)) {
		t.Error("allowed before a token came back")
	}
	if !l.Allow("alice", now.Add(time.Minute)) || l.Allow("alice", now.Add(time.Minute)) {
		t.Error("want exactly one event a minute later")
	}
	// A long rest refills to the burst, no further.
	later := now.Add(time.Hour)
	for i := range 3 {
		if !l.Allow("alice", later) {
			t.Fatalf("event %d after a rest refused", i+1)
		}
	}
	if l.Allow("alice", later) {
		t.Error("the bucket filled past the burst")
	}
	// A clock that steps back takes nothing away and gives nothing.
	if l.Allow("alice", later.Add(-time.Hour)) {
		t.Error("a clock step back refilled the bucket")
	}
}

func TestLimiterForgetsFullBuckets(t *testing.T) {
	l := New[int](time.Second, 1)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for i := range pruneAt {
		l.Allow(i, now)
	}
	l.Allow(-1, now.Add(time.Second)) // every earlier bucket is full again
	if n := len(l.buckets); n != 1 {
		t.Errorf("%d buckets kept, want only the newest", n)
	}
	l.Allow(-2, now.Add(time.Second))
	if !l.Exhausted(-1, now.Add(time.Second)) {
		t.Error("pruning forgot a bucket that was not full")
	}
}
