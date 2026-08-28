package idle

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/process"
)

const (
	// minPollInterval and maxPollInterval clamp how often a monitor wakes
	// while it has nothing scheduled (project not running, or countdown
	// parked). The interval scales with the idle timeout so second-scale
	// test timeouts stay responsive without waking 15-minute production
	// timeouts more than every few seconds.
	minPollInterval = 25 * time.Millisecond
	maxPollInterval = 5 * time.Second
	// wakeSlack is added when sleeping until a deadline so the monitor
	// wakes just past it instead of a hair before.
	wakeSlack = 10 * time.Millisecond
)

// Process is the view of a project's process supervisor the idle monitor
// needs. *process.Supervisor implements it.
type Process interface {
	// State returns the current lifecycle state without lifecycle locking.
	State() string
	// Snapshot returns lifecycle details; StartedAt anchors the idle window
	// of a project that has served no requests since it started.
	Snapshot() process.Snapshot
	// Stop gracefully stops the project's process group.
	Stop(ctx context.Context) error
}

// Monitor watches one project and stops it once it has been idle past its
// timeout. Run one Monitor goroutine per project that is subject to idle
// shutdown (always_on projects get none).
type Monitor struct {
	name     string
	proc     Process
	tracker  *Tracker
	timeout  time.Duration
	interval time.Duration
	logger   *log.Logger
}

// NewMonitor builds a monitor for one project. timeout is the project's
// effective idle timeout (config.Project.IdleTimeout).
func NewMonitor(name string, proc Process, tracker *Tracker, timeout time.Duration, logger *log.Logger) *Monitor {
	interval := timeout / 10
	if interval < minPollInterval {
		interval = minPollInterval
	}
	if interval > maxPollInterval {
		interval = maxPollInterval
	}
	return &Monitor{
		name:     name,
		proc:     proc,
		tracker:  tracker,
		timeout:  timeout,
		interval: interval,
		logger:   logger,
	}
}

// Run watches the project until ctx is cancelled (daemon shutdown). While
// the project is running and idle it sleeps until the current idle deadline;
// otherwise it polls at the monitor's interval. Waking at a stale deadline
// is always safe: the deadline only ever moves later (new activity extends
// it, a restarted process re-anchors it), so every wake just re-evaluates.
func (m *Monitor) Run(ctx context.Context) {
	timer := time.NewTimer(m.interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		timer.Reset(m.tick(ctx))
	}
}

// tick makes one idle decision — possibly stopping the project — and
// returns how long to sleep before the next one.
func (m *Monitor) tick(ctx context.Context) time.Duration {
	if m.proc.State() != process.StateRunning {
		return m.interval
	}
	now := time.Now()
	if m.tracker.Parked(now) {
		return m.interval
	}
	snap := m.proc.Snapshot()
	if snap.State != process.StateRunning || snap.StartedAt.IsZero() {
		return m.interval
	}
	if wait := m.tracker.Deadline(snap.StartedAt, m.timeout).Sub(now); wait > 0 {
		return wait + wakeSlack
	}

	// Deadline passed with no visible activity: arm the stop gate. BeginStop
	// re-checks activity after publishing the gate, so a request racing this
	// decision either aborts the stop here (ok == false) or is already
	// waiting on the gate for the stop to finish.
	release, ok := m.tracker.BeginStop()
	if !ok {
		return m.interval
	}
	defer release()

	// Re-evaluate under the armed gate: a manual stop or restart may have
	// raced the decision, and a restarted process earns a full idle window.
	now = time.Now()
	snap = m.proc.Snapshot()
	if snap.State != process.StateRunning || snap.StartedAt.IsZero() {
		return m.interval
	}
	if deadline := m.tracker.Deadline(snap.StartedAt, m.timeout); deadline.After(now) {
		return deadline.Sub(now) + wakeSlack
	}

	m.logger.Printf("project %q: idle for %s (idle timeout %s); stopping", m.name, m.idleFor(snap.StartedAt, now), m.timeout)
	if err := m.proc.Stop(ctx); err != nil && !errors.Is(err, context.Canceled) {
		m.logger.Printf("project %q: idle stop: %v", m.name, err)
	}
	return m.interval
}

// idleFor renders how long the project has been without activity.
func (m *Monitor) idleFor(startedAt, now time.Time) time.Duration {
	base := startedAt
	if la := m.tracker.LastActivity(); la.After(base) {
		base = la
	}
	return now.Sub(base).Round(100 * time.Millisecond)
}
