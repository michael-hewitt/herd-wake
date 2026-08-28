// Package idle implements idle shutdown (PRD §9): a per-project activity
// Tracker fed by the proxy and the control API, and a per-project Monitor
// that gracefully stops a project once it has seen no activity for its
// configured idle timeout.
//
// Activity is: in-flight HTTP requests, the most recent completed request,
// persistent connections (open WebSocket tunnels, fed by the proxy for
// projects with websockets_keep_alive enabled), and the manual activity
// lease. The idle countdown runs only while none of those is active; an
// in-flight request, an open persistent connection, or an unexpired lease
// parks it entirely, and every completed request (or closed connection)
// restarts the full timeout window.
package idle

import (
	"sync"
	"sync/atomic"
	"time"
)

// Tracker records one project's activity. All methods are safe for
// concurrent use; the request-path methods (RequestBegan, RequestEnded,
// StopGate) are lock-free so the proxy hot path never contends on a lock.
type Tracker struct {
	// inflight is the number of requests currently inside the proxy handler.
	inflight atomic.Int64
	// persistent counts open persistent connections (upgraded WebSocket
	// tunnels). The proxy feeds it for projects with websockets_keep_alive
	// enabled; while it is non-zero the idle countdown is parked.
	persistent atomic.Int64
	// lastActivity is the unix-nano time of the most recent completed
	// request or closed persistent connection (0 = none yet). It is written
	// BEFORE the matching counter is decremented, so an observer that sees
	// zero in-flight work also sees the completion time of every finished
	// piece of work.
	lastActivity atomic.Int64
	// gate is non-nil while an idle stop is in progress (armed by
	// BeginStop). Publishing the gate before checking the counters — while
	// the proxy increments its counter before reading the gate — is a
	// Dekker-style handshake: either the stop sees the request and aborts,
	// or the request sees the gate and waits for the stop to finish. A
	// request is therefore never forwarded to a process the idle monitor
	// has decided to stop.
	gate atomic.Pointer[stopGate]

	mu         sync.Mutex
	leaseUntil time.Time
}

type stopGate struct{ done chan struct{} }

// NewTracker returns an empty activity tracker.
func NewTracker() *Tracker { return &Tracker{} }

// RequestBegan marks one request in flight. Call it before doing anything
// else with the request; pair it with exactly one RequestEnded.
func (t *Tracker) RequestBegan() { t.inflight.Add(1) }

// RequestEnded records the request's completion time and releases its
// in-flight slot.
func (t *Tracker) RequestEnded() {
	t.lastActivity.Store(time.Now().UnixNano())
	t.inflight.Add(-1)
}

// PersistentOpened marks one persistent connection (e.g. a WebSocket) open.
// Pair with exactly one PersistentClosed.
func (t *Tracker) PersistentOpened() { t.persistent.Add(1) }

// PersistentClosed records the connection's close time and releases its slot.
func (t *Tracker) PersistentClosed() {
	t.lastActivity.Store(time.Now().UnixNano())
	t.persistent.Add(-1)
}

// StopGate returns a channel that is closed once the in-progress idle stop
// has finished, or nil when no idle stop is in progress. The proxy checks it
// after RequestBegan: a non-nil gate means the process is being torn down,
// so the request must wait for the gate and then cold-start instead of being
// forwarded to the dying process.
func (t *Tracker) StopGate() <-chan struct{} {
	if g := t.gate.Load(); g != nil {
		return g.done
	}
	return nil
}

// Inflight returns the number of requests currently in flight.
func (t *Tracker) Inflight() int64 { return t.inflight.Load() }

// Persistent returns the number of open persistent connections (WebSockets).
func (t *Tracker) Persistent() int64 { return t.persistent.Load() }

// LastActivity returns when the most recent request completed; zero when
// none has completed yet.
func (t *Tracker) LastActivity() time.Time {
	n := t.lastActivity.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Lease marks the project active until now+ttl, replacing any existing
// lease, and returns the expiry time. An active lease parks the idle
// countdown entirely; once it expires (or is released) normal idle rules
// resume.
func (t *Tracker) Lease(ttl time.Duration) time.Time {
	until := time.Now().Add(ttl)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.leaseUntil = until
	return until
}

// ReleaseLease clears the activity lease (a no-op when none is active).
func (t *Tracker) ReleaseLease() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.leaseUntil = time.Time{}
}

// LeaseUntil returns the lease expiry when a lease is active at now, zero
// otherwise.
func (t *Tracker) LeaseUntil(now time.Time) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.leaseUntil.After(now) {
		return t.leaseUntil
	}
	return time.Time{}
}

// Parked reports whether the idle countdown is parked at now: requests in
// flight, persistent connections open, or an unexpired lease.
func (t *Tracker) Parked(now time.Time) bool {
	return t.inflight.Load() > 0 ||
		t.persistent.Load() > 0 ||
		!t.LeaseUntil(now).IsZero()
}

// Deadline returns when the project may be idle-stopped: timeout after the
// most recent completed request, or after startedAt when nothing has
// completed since the process started — a freshly started project always
// gets a full idle window.
func (t *Tracker) Deadline(startedAt time.Time, timeout time.Duration) time.Time {
	base := startedAt
	if la := t.LastActivity(); la.After(base) {
		base = la
	}
	return base.Add(timeout)
}

// BeginStop arms the stop gate for an idle stop. It publishes the gate
// first and only then re-checks for activity, so it can never race a
// request into the dying process (see the gate field's comment): if any
// activity is visible the arm is rolled back and ok is false — the caller
// must not stop the project. On success the caller owns the stop and must
// call release exactly once, after the stop has fully finished, to let
// waiting requests proceed (into a cold start).
func (t *Tracker) BeginStop() (release func(), ok bool) {
	g := &stopGate{done: make(chan struct{})}
	if !t.gate.CompareAndSwap(nil, g) {
		return nil, false // another stop is already in progress
	}
	if t.Parked(time.Now()) {
		t.gate.Store(nil)
		close(g.done) // free any request that saw the short-lived gate
		return nil, false
	}
	return func() {
		t.gate.Store(nil)
		close(g.done)
	}, true
}
