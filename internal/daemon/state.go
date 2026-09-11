package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/idle"
	"github.com/michael-hewitt/herd-wake/internal/process"
	"github.com/michael-hewitt/herd-wake/internal/proxy"
	"github.com/michael-hewitt/herd-wake/internal/version"
)

// Project lifecycle states, re-exported from the process package for
// convenience in daemon-level code and tests.
const (
	StateStopped  = process.StateStopped
	StateStarting = process.StateStarting
	StateRunning  = process.StateRunning
	StateStopping = process.StateStopping
	StateFailed   = process.StateFailed
)

// projectState is the daemon's runtime record for one registered project:
// its configuration, the supervisor owning its process lifecycle, the
// activity tracker driving its idle shutdown, its slot in a listener's
// router, and the listener itself. A reload that changes a project builds
// a fresh record (new supervisor for the new config) but keeps the tracker
// — leases and activity history survive — and keeps the route when the
// address and host did not change.
type projectState struct {
	project *config.Project
	proc    *process.Supervisor
	tracker *idle.Tracker
	// sw is the project's handler slot in its listener's router; nil until
	// Run (or the reload that added the project) routes it.
	sw *handlerSwitch
	// bind is the listener the project is served on; nil until attached.
	bind *binding
	// deactivate ends the project's idle monitor (or always_on starter);
	// nil until activate.
	deactivate context.CancelFunc
	// dynamic marks a project a wildcard entry materialised at runtime;
	// entry is that entry.
	dynamic bool
	entry   *entryState
	// retireOnce guards retire: a dynamic project may be retired by a
	// reload and by a drop (vanished directory) at the same time.
	retireOnce sync.Once
}

// newProjectState builds the runtime record for p with the given activity
// tracker; nothing is bound or started yet.
func (d *Daemon) newProjectState(p *config.Project, tracker *idle.Tracker) *projectState {
	return &projectState{
		project: p,
		proc:    process.NewSupervisor(p, d.logDir, d.logger),
		tracker: tracker,
	}
}

// handler builds the project's on-demand proxy handler.
func (d *Daemon) handler(st *projectState) http.Handler {
	upstream := onDemandUpstream{Supervisor: st.proc, draining: &d.draining}
	return proxy.NewOnDemand(st.project, upstream, st.tracker, d.logger)
}

// sortedStates returns the registered projects in name order.
func (d *Daemon) sortedStates() []*projectState {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.sortedStatesLocked()
}

// sortedStatesLocked is sortedStates with d.mu already held.
func (d *Daemon) sortedStatesLocked() []*projectState {
	names := make([]string, 0, len(d.states))
	for name := range d.states {
		names = append(names, name)
	}
	sort.Strings(names)
	states := make([]*projectState, 0, len(names))
	for _, name := range names {
		states = append(states, d.states[name])
	}
	return states
}

// findProject looks up a registered project by name.
func (d *Daemon) findProject(name string) (*projectState, error) {
	d.mu.RLock()
	st, ok := d.states[name]
	d.mu.RUnlock()
	if ok {
		return st, nil
	}
	return nil, fmt.Errorf("%w %q (run `herd-wake projects` to list registered projects)",
		control.ErrUnknownProject, name)
}

// projectStatus renders one project's control-API status from its supervisor
// snapshot.
func (d *Daemon) projectStatus(st *projectState) control.ProjectStatus {
	snap := st.proc.Snapshot()
	now := time.Now()
	status := control.ProjectStatus{
		Name:             st.project.Name,
		PublicURL:        st.project.PublicURL,
		Host:             st.project.Host,
		SupervisorPort:   st.project.SupervisorPort,
		ApplicationPort:  st.project.ApplicationPort,
		WorkingDirectory: st.project.WorkingDirectory,
		Source:           st.project.Source,
		Dynamic:          st.dynamic,
		State:            snap.State,
		PID:              snap.PID,
		LastExit:         snap.LastExit,
		LastError:        snap.LastError,
		AlwaysOn:         st.project.AlwaysOn,
		InflightRequests: st.tracker.Inflight(),
		LastActivityAt:   st.tracker.LastActivity(),
		LeaseUntil:       st.tracker.LeaseUntil(now),
	}
	if !snap.StartedAt.IsZero() {
		status.UptimeSeconds = now.Sub(snap.StartedAt).Seconds()
	}
	// A scheduled idle stop only exists for a running, non-always_on project
	// whose countdown is not parked by in-flight requests or a lease.
	if snap.State == StateRunning && !snap.StartedAt.IsZero() &&
		!st.project.AlwaysOn && !st.tracker.Parked(now) {
		status.IdleStopAt = st.tracker.Deadline(snap.StartedAt, st.project.IdleTimeout())
	}
	return status
}

// Status implements control.Provider.
func (d *Daemon) Status() control.StatusResponse {
	d.mu.RLock()
	defer d.mu.RUnlock()
	resp := control.StatusResponse{
		Version:       version.String(),
		PID:           os.Getpid(),
		StartedAt:     d.startedAt,
		UptimeSeconds: time.Since(d.startedAt).Seconds(),
		ConfigPath:    d.configPath,
		LastReloadAt:  d.lastReloadAt,
		Projects:      make([]control.ProjectStatus, 0, len(d.states)),
	}
	for _, st := range d.sortedStatesLocked() {
		resp.Projects = append(resp.Projects, d.projectStatus(st))
	}
	for _, es := range d.sortedEntriesLocked() {
		resp.Wildcards = append(resp.Wildcards, d.wildcardStatusLocked(es))
	}
	return resp
}

// StartProject implements control.Provider: it starts the named project and
// returns once it is running or its startup failed.
func (d *Daemon) StartProject(ctx context.Context, name string) (control.ProjectStatus, error) {
	st, err := d.findProject(name)
	if err != nil {
		return control.ProjectStatus{}, err
	}
	select {
	case err := <-st.proc.EnsureStarted():
		if err != nil {
			return control.ProjectStatus{}, err
		}
		return d.projectStatus(st), nil
	case <-ctx.Done():
		return control.ProjectStatus{}, ctx.Err()
	}
}

// StopProject implements control.Provider: it gracefully stops the named
// project's process group (force-killing only after its shutdown timeout).
// A dynamic project whose worktree directory has vanished is dropped from
// the table once stopped.
func (d *Daemon) StopProject(ctx context.Context, name string) (control.ProjectStatus, error) {
	st, err := d.findProject(name)
	if err != nil {
		return control.ProjectStatus{}, err
	}
	if err := st.proc.Stop(ctx); err != nil {
		return control.ProjectStatus{}, err
	}
	status := d.projectStatus(st)
	if st.dynamic {
		d.dropIfVanished(st)
	}
	return status, nil
}

// RestartProject implements control.Provider: stop (if needed), then start.
// A dynamic project re-runs port_command first (see restartDynamic).
func (d *Daemon) RestartProject(ctx context.Context, name string) (control.ProjectStatus, error) {
	st, err := d.findProject(name)
	if err != nil {
		return control.ProjectStatus{}, err
	}
	if st.dynamic {
		return d.restartDynamic(ctx, st)
	}
	if err := st.proc.Restart(ctx); err != nil {
		return control.ProjectStatus{}, err
	}
	return d.projectStatus(st), nil
}

// LeaseProject implements control.Provider: it marks the named project
// active for ttl, parking its idle countdown until the lease expires or is
// released. Leasing does not start a stopped project.
func (d *Daemon) LeaseProject(_ context.Context, name string, ttl time.Duration) (control.ProjectStatus, error) {
	st, err := d.findProject(name)
	if err != nil {
		return control.ProjectStatus{}, err
	}
	until := st.tracker.Lease(ttl)
	d.logger.Printf("project %q: activity lease until %s (ttl %s)", name, until.Format(time.RFC3339), ttl)
	return d.projectStatus(st), nil
}

// ReleaseProjectLease implements control.Provider: it clears the named
// project's activity lease, so normal idle rules apply again.
func (d *Daemon) ReleaseProjectLease(_ context.Context, name string) (control.ProjectStatus, error) {
	st, err := d.findProject(name)
	if err != nil {
		return control.ProjectStatus{}, err
	}
	st.tracker.ReleaseLease()
	d.logger.Printf("project %q: activity lease released", name)
	return d.projectStatus(st), nil
}

// ProjectLogs implements control.Provider: recent captured output for one
// project.
func (d *Daemon) ProjectLogs(name string, maxLines int) (control.LogsResponse, error) {
	st, err := d.findProject(name)
	if err != nil {
		return control.LogsResponse{}, err
	}
	return control.LogsResponse{
		Name:    name,
		LogFile: st.proc.LogPath(),
		Lines:   st.proc.Logs(maxLines),
	}, nil
}

// retiring is one project being taken out of service by a reload, a drop,
// or daemon shutdown, and whether its route is kept for a replacement on
// the same address and host.
type retiring struct {
	st          *projectState
	keepBinding bool
}

// retireBound is how long retiring the given projects may take: the
// longest shutdown timeout plus the force-kill drain.
func retireBound(items []retiring) time.Duration {
	maxWait := 15 * time.Second
	for _, item := range items {
		if wait := time.Duration(item.st.project.ShutdownTimeoutSeconds)*time.Second + 15*time.Second; wait > maxWait {
			maxWait = wait
		}
	}
	return maxWait
}

// retire takes one project out of service: its monitor is stopped, its
// route removed (unless kept for a replacement) — closing its listener
// when nothing else is on it — its supervisor retired so nothing can
// respawn it, and its process group stopped gracefully. Removing the route
// first means a removed port (or host) stops answering before its process
// goes away; retiring before stopping means a request that slipped past
// the closing route cannot restart what the stop is ending. It runs at
// most once per project.
func (d *Daemon) retire(ctx context.Context, item retiring) {
	st := item.st
	st.retireOnce.Do(func() {
		if st.deactivate != nil {
			st.deactivate()
		}
		d.detach(st, item.keepBinding)
		st.proc.Retire()
		if err := st.proc.Stop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			d.logger.Printf("project %q: stop while retiring: %v", st.project.Name, err)
		}
	})
}

// retireAll retires the given projects in parallel and returns once every
// process group is gone (or the bound elapsed). It is idempotent:
// supervisors that never started anything are no-ops, and only tracked
// PGIDs are ever signaled.
func (d *Daemon) retireAll(items []retiring) {
	ctx, cancel := context.WithTimeout(context.Background(), retireBound(items))
	defer cancel()

	var wg sync.WaitGroup
	for _, item := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.retire(ctx, item)
		}()
	}
	wg.Wait()
}

// stopAll closes every project listener and gracefully stops every
// supervised process group, dynamic projects included. It runs on every
// daemon exit — including the panic path via defer — and waits for any
// reload in progress to finish first, so a project a reload is just adding
// is stopped too.
func (d *Daemon) stopAll() {
	d.reloadMu.Lock()
	defer d.reloadMu.Unlock()
	states := d.sortedStates()
	items := make([]retiring, 0, len(states))
	for _, st := range states {
		items = append(items, retiring{st: st})
	}
	d.retireAll(items)
	for _, es := range d.sortedEntries() {
		if es.bind != nil {
			d.unbindEntry(es)
		}
	}
}
