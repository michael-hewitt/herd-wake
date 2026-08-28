package idle

import (
	"context"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/process"
)

// fakeProcess implements Process for monitor tests: a settable state plus a
// stop counter. Stop transitions the fake to stopped, like the real thing.
type fakeProcess struct {
	mu        sync.Mutex
	state     string
	startedAt time.Time
	stops     atomic.Int64
}

func newFakeProcess(state string, startedAt time.Time) *fakeProcess {
	return &fakeProcess{state: state, startedAt: startedAt}
}

func (f *fakeProcess) State() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeProcess) Snapshot() process.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap := process.Snapshot{State: f.state}
	if f.state == process.StateRunning {
		snap.StartedAt = f.startedAt
	}
	return snap
}

func (f *fakeProcess) Stop(context.Context) error {
	f.stops.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = process.StateStopped
	f.startedAt = time.Time{}
	return nil
}

func (f *fakeProcess) setRunning(startedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = process.StateRunning
	f.startedAt = startedAt
}

func runMonitor(t *testing.T, m *Monitor) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("monitor did not stop after cancel")
		}
	})
}

func waitStops(t *testing.T, f *fakeProcess, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for f.stops.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("stops = %d after %s, want %d", f.stops.Load(), timeout, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func TestMonitorStopsIdleProject(t *testing.T) {
	proc := newFakeProcess(process.StateRunning, time.Now())
	tr := NewTracker()
	runMonitor(t, NewMonitor("p", proc, tr, 150*time.Millisecond, testLogger()))

	waitStops(t, proc, 1, 5*time.Second)

	// One stop only: a stopped project is left alone.
	time.Sleep(300 * time.Millisecond)
	if got := proc.stops.Load(); got != 1 {
		t.Errorf("stops = %d, want exactly 1", got)
	}
}

func TestMonitorLeavesStoppedProjectAlone(t *testing.T) {
	proc := newFakeProcess(process.StateStopped, time.Time{})
	tr := NewTracker()
	runMonitor(t, NewMonitor("p", proc, tr, 50*time.Millisecond, testLogger()))

	time.Sleep(300 * time.Millisecond)
	if got := proc.stops.Load(); got != 0 {
		t.Errorf("stops = %d, want 0 for a project that is not running", got)
	}
}

func TestMonitorInflightRequestParksIndefinitely(t *testing.T) {
	proc := newFakeProcess(process.StateRunning, time.Now())
	tr := NewTracker()
	tr.RequestBegan() // a long-running request
	runMonitor(t, NewMonitor("p", proc, tr, 100*time.Millisecond, testLogger()))

	// Far past the timeout, the project must still be running.
	time.Sleep(500 * time.Millisecond)
	if got := proc.stops.Load(); got != 0 {
		t.Fatalf("stops = %d while a request is in flight, want 0", got)
	}

	// Completion restarts a full idle window, then the stop happens.
	tr.RequestEnded()
	waitStops(t, proc, 1, 5*time.Second)
}

func TestMonitorActivityExtendsWindow(t *testing.T) {
	proc := newFakeProcess(process.StateRunning, time.Now())
	tr := NewTracker()
	runMonitor(t, NewMonitor("p", proc, tr, 400*time.Millisecond, testLogger()))

	// Touch the project a few times inside the window: no stop may happen
	// while activity keeps arriving.
	for range 4 {
		time.Sleep(150 * time.Millisecond)
		tr.RequestBegan()
		tr.RequestEnded()
	}
	if got := proc.stops.Load(); got != 0 {
		t.Fatalf("stops = %d during active traffic, want 0", got)
	}

	waitStops(t, proc, 1, 5*time.Second)
}

func TestMonitorLeaseParksUntilExpiry(t *testing.T) {
	proc := newFakeProcess(process.StateRunning, time.Now())
	tr := NewTracker()
	tr.Lease(600 * time.Millisecond)
	runMonitor(t, NewMonitor("p", proc, tr, 100*time.Millisecond, testLogger()))

	time.Sleep(400 * time.Millisecond) // well past the timeout, inside the lease
	if got := proc.stops.Load(); got != 0 {
		t.Fatalf("stops = %d during an active lease, want 0", got)
	}

	waitStops(t, proc, 1, 5*time.Second) // lease expired: stop follows
}

func TestMonitorRestartedProcessGetsFullWindow(t *testing.T) {
	proc := newFakeProcess(process.StateRunning, time.Now())
	tr := NewTracker()
	runMonitor(t, NewMonitor("p", proc, tr, 200*time.Millisecond, testLogger()))

	waitStops(t, proc, 1, 5*time.Second)

	// A restart (fresh StartedAt) earns a fresh idle window and one more
	// idle stop — not an instant kill from the stale deadline.
	restartedAt := time.Now()
	proc.setRunning(restartedAt)
	waitStops(t, proc, 2, 5*time.Second)
	if stopped := time.Since(restartedAt); stopped < 200*time.Millisecond {
		t.Errorf("restarted process was stopped after %s, want at least the full 200ms window", stopped)
	}
}
