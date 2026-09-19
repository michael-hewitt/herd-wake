package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/process"
	"github.com/michael-hewitt/herd-wake/internal/proxy"
)

// The running-server budget: max_running caps how many projects may be
// starting or running at once — the top-level cap across everything the
// daemon supervises, and optionally a per-entry cap on a wildcard entry's
// worktrees. Every start (request-triggered, project:start/restart, and
// always_on) reserves a slot first: with no cap set that is a no-op, and
// with room under every applicable cap it is one lock and a count.
//
// When a start would exceed a cap, the least-recently-active running project
// is evicted first — the same graceful whole-group stop an idle stop does,
// behind the same stop gate, so a request racing the eviction waits and
// cold-starts instead of reaching the dying process. A project that is
// always_on, has requests in flight, holds open protected WebSockets, or
// holds a lease is never evicted. If nothing can be evicted the start waits
// (bounded by hold_max_wait_seconds) for a slot to free up — a project
// idling out, a request finishing — and then fails with a *CapacityError
// naming the projects holding the slots and why each cannot be evicted.
//
// Reservation, eviction, and the start it makes room for are serialized by
// budgetMu, so two simultaneous wake-ups never both conclude there is room.
// Only cold starts take that lock: requests to running projects never do.
// A project counts while starting or running; it stops counting the moment
// its stop begins (the eviction's stop runs to completion in the
// background), and an evicted project is simply stopped — its next request
// cold-starts it as usual.
const (
	// budgetPollInterval is how often a start waiting for a free slot
	// re-checks the budget.
	budgetPollInterval = 100 * time.Millisecond
	// capacityWaitMargin is taken off hold_max_wait_seconds for a
	// request-triggered start's capacity wait, so a request whose slot never
	// frees up gets the capacity diagnostic rather than the proxy's generic
	// hold timeout, which is measured from slightly earlier.
	capacityWaitMargin = time.Second
	// evictionAttempts bounds how often a reservation re-counts after the
	// projects it saw as blockers all changed state underneath it.
	evictionAttempts = 3
)

// CapacityError is why a start was refused: every running-server slot under
// the cap named by Scope is held by a project that cannot be evicted.
type CapacityError struct {
	// Project is the project that could not start.
	Project string
	// Scope names the cap that is full: "max_running" for the top-level
	// budget, or `discovery "<entry>": max_running` for a per-entry cap.
	Scope string
	// Running and Max are the slots taken and the cap.
	Running, Max int
	// Blockers are the projects holding the slots, least recently active
	// first, each with why it cannot be evicted.
	Blockers []Blocker
}

// Blocker is one project holding a running-server slot that cannot be
// evicted, and why.
type Blocker struct {
	Name   string
	Reason string
}

func (e *CapacityError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "no running-server slot for project %q: %d of %d are taken (%s) and none can be freed",
		e.Project, e.Running, e.Max, e.Scope)
	if len(e.Blockers) > 0 {
		b.WriteString(": ")
		for i, blocker := range e.Blockers {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q %s", blocker.Name, blocker.Reason)
		}
	}
	return b.String()
}

// DiagnosticTitle implements proxy.Explained.
func (e *CapacityError) DiagnosticTitle() string {
	return fmt.Sprintf("no running-server slot for %q (capacity)", e.Project)
}

// DiagnosticHint implements proxy.Explained.
func (e *CapacityError) DiagnosticHint() string {
	return "Stop one of them (`herd-wake project:stop <name>`), wait for one to finish or idle out and retry, or raise max_running in the config and run `herd-wake reload`."
}

// budgeted reports whether any cap applies to st.
func (d *Daemon) budgeted(st *projectState) bool {
	if d.maxRunning.Load() > 0 {
		return true
	}
	return st.entry != nil && st.entry.maxRunning.Load() > 0
}

// capacityWait is how long a start of p may wait for a running-server slot.
// Request-triggered starts leave a margin under the proxy's own hold bound
// (see capacityWaitMargin).
func capacityWait(p *config.Project, onDemand bool) time.Duration {
	wait := proxy.HoldMaxWait(p)
	if onDemand {
		wait -= capacityWaitMargin
		if wait < capacityWaitMargin {
			wait = capacityWaitMargin
		}
	}
	return wait
}

// runContext returns Run's lifetime context (Background before Run).
func (d *Daemon) runContext() context.Context {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.runCtx == nil {
		return context.Background()
	}
	return d.runCtx
}

// startOnDemand is a request-triggered start of st: the supervisor's
// single-flight startup, behind a slot reservation when a cap applies. It
// returns a channel yielding the outcome exactly once — a *CapacityError
// when no slot could be freed within the project's capacity wait. Starts
// the supervisor would refuse or short-circuit anyway (already starting or
// running, stopping, retired, or inside its failure backoff) never reserve
// a slot, so nothing is evicted for them.
func (d *Daemon) startOnDemand(st *projectState) <-chan error {
	if !d.budgeted(st) {
		return st.proc.EnsureStartedOnDemand()
	}
	switch st.proc.State() {
	case StateRunning, StateStarting, StateStopping:
		return st.proc.EnsureStartedOnDemand()
	}
	if snap := st.proc.Snapshot(); snap.State == StateFailed && time.Now().Before(snap.NextRetryAt) {
		return st.proc.EnsureStartedOnDemand()
	}

	// The common case — a free slot, or one eviction — completes here, so the
	// startup is in flight when the channel is handed back, exactly like an
	// unbudgeted start.
	d.budgetMu.Lock()
	ch, capErr := d.tryReserveLocked(st, st.proc.EnsureStartedOnDemand)
	d.budgetMu.Unlock()
	if capErr == nil {
		return ch
	}

	done := make(chan error, 1)
	go func() {
		ch, err := d.reserve(d.runContext(), st, capacityWait(st.project, true), st.proc.EnsureStartedOnDemand)
		if err != nil {
			done <- err
			return
		}
		done <- <-ch
	}()
	return done
}

// startManual is a manual start of st (project:start, project:restart,
// always_on): it reserves a slot (waiting up to the project's capacity
// wait, bounded by ctx), starts the project, and returns once it is running
// or its startup failed.
func (d *Daemon) startManual(ctx context.Context, st *projectState) error {
	ch, err := d.reserve(ctx, st, capacityWait(st.project, false), st.proc.EnsureStarted)
	if err != nil {
		return err
	}
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// restart stops st (if needed) and starts it again through the budget.
func (d *Daemon) restart(ctx context.Context, st *projectState) error {
	if err := st.proc.Stop(ctx); err != nil {
		return err
	}
	return d.startManual(ctx, st)
}

// reserve runs start under the budget lock once st may hold a slot: at
// once when there is room (or no cap applies), after evicting what it
// takes, or after waiting up to wait — re-trying as projects free up — and
// otherwise fails with the last *CapacityError. ctx ending early (daemon
// shutdown, the caller going away) returns its error instead.
func (d *Daemon) reserve(ctx context.Context, st *projectState, wait time.Duration, start func() <-chan error) (<-chan error, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(budgetPollInterval)
	defer poll.Stop()
	for {
		d.budgetMu.Lock()
		ch, capErr := d.tryReserveLocked(st, start)
		d.budgetMu.Unlock()
		if capErr == nil {
			return ch, nil
		}
		select {
		case <-deadline.C:
			return nil, capErr
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-poll.C:
		}
	}
}

// tryReserveLocked is one reservation attempt with budgetMu held: it counts
// the projects holding slots, evicts as many evictable ones as it takes to
// make room (the most recently active are kept), and calls start — whose
// synchronous state transition is what claims the slot — while the lock
// still holds the count steady. A *CapacityError means no room and nothing
// evictable right now.
func (d *Daemon) tryReserveLocked(st *projectState, start func() <-chan error) (<-chan error, *CapacityError) {
	switch st.proc.State() {
	case StateRunning, StateStarting, StateStopping:
		return start(), nil // holds a slot already, or start refuses anyway
	}
	global := int(d.maxRunning.Load())
	entryCap := 0
	if st.entry != nil {
		entryCap = int(st.entry.maxRunning.Load())
	}
	if global == 0 && entryCap == 0 {
		return start(), nil
	}
	d.mu.RLock()
	registered := d.states[st.project.Name] == st
	states := d.sortedStatesLocked()
	d.mu.RUnlock()
	if !registered {
		return start(), nil // retired meanwhile; start reports it
	}

	var lastErr *CapacityError
	for attempt := 0; attempt < evictionAttempts; attempt++ {
		globalCount, entryCount := countRunning(states, st.entry)
		needGlobal := global > 0 && globalCount >= global
		needEntry := entryCap > 0 && entryCount >= entryCap
		if !needGlobal && !needEntry {
			return start(), nil
		}
		pool := states
		capErr := &CapacityError{Project: st.project.Name, Scope: "max_running", Running: globalCount, Max: global}
		if needEntry {
			// A victim from the entry frees a slot under both caps.
			pool = entryStates(states, st.entry)
			capErr = &CapacityError{
				Project: st.project.Name, Running: entryCount, Max: entryCap,
				Scope: fmt.Sprintf("discovery %q: max_running", st.entry.entry.Name),
			}
		}
		victim, blockers := d.evict(pool, st, capErr)
		if victim != nil {
			continue // one slot freed; there may be more to free
		}
		capErr.Blockers = blockers
		lastErr = capErr
		if len(blockers) > 0 {
			return nil, capErr
		}
		// Everything counted changed state before it could be judged; count
		// again.
	}
	return nil, lastErr
}

// holdsSlot reports whether a project counts against the budget.
func holdsSlot(st *projectState) bool {
	switch st.proc.State() {
	case StateRunning, StateStarting:
		return true
	}
	return false
}

// countRunning counts the projects holding slots: all of them, and those of
// entry (0 when entry is nil).
func countRunning(states []*projectState, entry *entryState) (global, ofEntry int) {
	for _, st := range states {
		if !holdsSlot(st) {
			continue
		}
		global++
		if entry != nil && st.entry == entry {
			ofEntry++
		}
	}
	return global, ofEntry
}

// entryStates filters states to entry's dynamic projects.
func entryStates(states []*projectState, entry *entryState) []*projectState {
	var out []*projectState
	for _, st := range states {
		if st.dynamic && st.entry == entry {
			out = append(out, st)
		}
	}
	return out
}

// candidate is one project holding a slot, ranked for eviction.
type candidate struct {
	st         *projectState
	snap       process.Snapshot
	lastActive time.Time
}

// evictionOrder lists the projects among states that hold a slot, least
// recently active first: by the completion of their last request (or
// closed WebSocket), or their start time when nothing has completed since.
func evictionOrder(states []*projectState) []candidate {
	var out []candidate
	for _, st := range states {
		if !holdsSlot(st) {
			continue
		}
		snap := st.proc.Snapshot()
		switch snap.State {
		case StateRunning, StateStarting:
		default:
			continue
		}
		last := snap.StartedAt
		if la := st.tracker.LastActivity(); la.After(last) {
			last = la
		}
		out = append(out, candidate{st: st, snap: snap, lastActive: last})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].lastActive.Before(out[j].lastActive) })
	return out
}

// blockReason says why a slot-holding project cannot be evicted right now,
// or "" when it can (running, not always_on, nothing parking it).
func blockReason(c candidate, now time.Time) string {
	switch {
	case c.st.project.AlwaysOn:
		return "is always_on"
	case c.snap.State == StateStarting:
		return "is still starting"
	}
	tr := c.st.tracker
	switch {
	case tr.Inflight() > 0:
		return fmt.Sprintf("has %d request(s) in flight", tr.Inflight())
	case tr.Persistent() > 0:
		return fmt.Sprintf("has %d open WebSocket(s)", tr.Persistent())
	case !tr.LeaseUntil(now).IsZero():
		return "is leased until " + tr.LeaseUntil(now).Local().Format("15:04:05")
	}
	return ""
}

// evict stops the least-recently-active evictable project in pool to make
// room for st, returning it, or nil with why each slot holder could not be
// evicted. The victim's stop gate is armed first (exactly as an idle stop
// does), so a request racing the eviction waits for the stop and then
// cold-starts; the stop itself finishes in the background, and a dynamic
// project whose worktree vanished meanwhile is dropped once stopped.
func (d *Daemon) evict(pool []*projectState, st *projectState, budget *CapacityError) (*projectState, []Blocker) {
	var blockers []Blocker
	now := time.Now()
	for _, c := range evictionOrder(pool) {
		if reason := blockReason(c, now); reason != "" {
			blockers = append(blockers, Blocker{Name: c.st.project.Name, Reason: reason})
			continue
		}
		release, ok := c.st.tracker.BeginStop()
		if !ok {
			// Parked between the check and the gate, or an idle stop is
			// already stopping it (its slot frees up in a moment).
			reason := blockReason(c, time.Now())
			if reason == "" {
				reason = "is being idle-stopped"
			}
			blockers = append(blockers, Blocker{Name: c.st.project.Name, Reason: reason})
			continue
		}
		victim := c.st
		d.logger.Printf("project %q: evicting (least recently active, idle for %s) to make room for %q: %d of %d running-server slots taken (%s)",
			victim.project.Name, now.Sub(c.lastActive).Round(100*time.Millisecond), st.project.Name, budget.Running, budget.Max, budget.Scope)
		done := victim.proc.StopAsync() // the slot is free once this returns
		go func() {
			<-done
			release()
			if victim.dynamic {
				d.dropIfVanished(victim)
			}
		}()
		return victim, nil
	}
	return nil, blockers
}

// nextEviction names the project that would be evicted first if a start
// needed a slot among states right now; "" when nothing is evictable.
func nextEviction(states []*projectState) string {
	now := time.Now()
	for _, c := range evictionOrder(states) {
		if blockReason(c, now) == "" {
			return c.st.project.Name
		}
	}
	return ""
}

// budgetStatusLocked renders the top-level budget for the control API.
// Called with d.mu held.
func (d *Daemon) budgetStatusLocked() *control.BudgetStatus {
	states := d.sortedStatesLocked()
	running, _ := countRunning(states, nil)
	return &control.BudgetStatus{
		MaxRunning:   int(d.maxRunning.Load()),
		Running:      running,
		NextEviction: nextEviction(states),
	}
}

// applyBudget installs the caps of a (re)loaded config live: the top-level
// max_running, and the per-entry cap of every wildcard entry the reload
// kept. Lowering a cap below the current count stops nothing; the next
// start that needs a slot evicts down to the cap.
func (d *Daemon) applyBudget(cfg *config.Config, kept map[string]*entryState) {
	if prev := d.maxRunning.Swap(int64(cfg.MaxRunning)); prev != int64(cfg.MaxRunning) {
		d.logger.Printf("max_running: %s (was %s)", describeCap(cfg.MaxRunning), describeCap(int(prev)))
	}
	for _, e := range cfg.Discovery {
		es := kept[e.Name]
		if es == nil {
			continue
		}
		if prev := es.maxRunning.Swap(int64(e.MaxRunning)); prev != int64(e.MaxRunning) {
			d.logger.Printf("discovery %q: max_running: %s (was %s)", e.Name, describeCap(e.MaxRunning), describeCap(int(prev)))
		}
	}
}

// describeCap renders a cap for log lines.
func describeCap(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return fmt.Sprint(n)
}
