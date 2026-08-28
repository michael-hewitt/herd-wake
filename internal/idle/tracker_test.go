package idle

import (
	"testing"
	"time"
)

func TestTrackerRequestLifecycle(t *testing.T) {
	tr := NewTracker()
	now := time.Now()

	if tr.Inflight() != 0 || !tr.LastActivity().IsZero() || tr.Parked(now) {
		t.Fatalf("fresh tracker: inflight=%d lastActivity=%v parked=%v, want idle and empty",
			tr.Inflight(), tr.LastActivity(), tr.Parked(now))
	}

	tr.RequestBegan()
	if tr.Inflight() != 1 || !tr.Parked(now) {
		t.Errorf("after RequestBegan: inflight=%d parked=%v, want 1 and parked", tr.Inflight(), tr.Parked(now))
	}
	if !tr.LastActivity().IsZero() {
		t.Errorf("LastActivity should stay zero until a request completes; got %v", tr.LastActivity())
	}

	before := time.Now()
	tr.RequestEnded()
	if tr.Inflight() != 0 || tr.Parked(time.Now()) {
		t.Errorf("after RequestEnded: inflight=%d, want 0 and not parked", tr.Inflight())
	}
	if la := tr.LastActivity(); la.Before(before) {
		t.Errorf("LastActivity = %v, want at or after %v", la, before)
	}
}

func TestTrackerPersistentConnectionsPark(t *testing.T) {
	tr := NewTracker()
	tr.PersistentOpened()
	if !tr.Parked(time.Now()) {
		t.Error("an open persistent connection should park the countdown")
	}
	tr.PersistentClosed()
	if tr.Parked(time.Now()) {
		t.Error("closing the persistent connection should unpark the countdown")
	}
	if tr.LastActivity().IsZero() {
		t.Error("closing a persistent connection should count as activity")
	}
}

func TestTrackerLease(t *testing.T) {
	tr := NewTracker()
	now := time.Now()

	until := tr.Lease(time.Hour)
	if want := now.Add(time.Hour); until.Before(want.Add(-time.Second)) || until.After(want.Add(time.Second)) {
		t.Errorf("Lease expiry = %v, want about %v", until, want)
	}
	if got := tr.LeaseUntil(time.Now()); !got.Equal(until) {
		t.Errorf("LeaseUntil = %v, want %v", got, until)
	}
	if !tr.Parked(time.Now()) {
		t.Error("an active lease should park the countdown")
	}
	// An expired lease no longer counts.
	if got := tr.LeaseUntil(until.Add(time.Minute)); !got.IsZero() {
		t.Errorf("LeaseUntil after expiry = %v, want zero", got)
	}
	if tr.Parked(until.Add(time.Minute)) {
		t.Error("an expired lease should not park the countdown")
	}

	// A new lease replaces the old one; release clears it.
	shorter := tr.Lease(time.Minute)
	if got := tr.LeaseUntil(time.Now()); !got.Equal(shorter) {
		t.Errorf("re-lease: LeaseUntil = %v, want %v", got, shorter)
	}
	tr.ReleaseLease()
	if got := tr.LeaseUntil(time.Now()); !got.IsZero() {
		t.Errorf("LeaseUntil after release = %v, want zero", got)
	}
}

func TestTrackerDeadline(t *testing.T) {
	tr := NewTracker()
	startedAt := time.Now().Add(-time.Minute)
	timeout := 10 * time.Second

	// No completed request yet: the window is anchored at process start.
	if got, want := tr.Deadline(startedAt, timeout), startedAt.Add(timeout); !got.Equal(want) {
		t.Errorf("Deadline with no activity = %v, want %v", got, want)
	}

	// A completed request after start re-anchors the window.
	tr.RequestBegan()
	tr.RequestEnded()
	got := tr.Deadline(startedAt, timeout)
	if want := tr.LastActivity().Add(timeout); !got.Equal(want) {
		t.Errorf("Deadline after activity = %v, want %v", got, want)
	}
}

func TestTrackerBeginStop(t *testing.T) {
	tr := NewTracker()

	// Parked by an in-flight request: the stop must not be granted, and the
	// short-lived gate must end up released.
	tr.RequestBegan()
	if _, ok := tr.BeginStop(); ok {
		t.Fatal("BeginStop with a request in flight should refuse")
	}
	if tr.StopGate() != nil {
		t.Fatal("a refused BeginStop must not leave a gate armed")
	}
	tr.RequestEnded()

	// Parked by a lease: same.
	tr.Lease(time.Hour)
	if _, ok := tr.BeginStop(); ok {
		t.Fatal("BeginStop with an active lease should refuse")
	}
	tr.ReleaseLease()

	// Idle: the stop is granted, the gate is armed until released.
	release, ok := tr.BeginStop()
	if !ok {
		t.Fatal("BeginStop while idle should succeed")
	}
	gate := tr.StopGate()
	if gate == nil {
		t.Fatal("StopGate should be armed during a stop")
	}
	select {
	case <-gate:
		t.Fatal("gate must stay open until release")
	default:
	}
	// Only one stop at a time.
	if _, ok := tr.BeginStop(); ok {
		t.Fatal("a second BeginStop during a stop should refuse")
	}
	release()
	select {
	case <-gate:
	default:
		t.Fatal("release must close the gate")
	}
	if tr.StopGate() != nil {
		t.Error("no gate should remain after release")
	}
}
