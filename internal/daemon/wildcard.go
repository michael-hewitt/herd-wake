package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/discovery"
	"github.com/michael-hewitt/herd-wake/internal/idle"
	"github.com/michael-hewitt/herd-wake/internal/proxy"
)

// Resolution backoff: after a label fails to resolve (port_command failed,
// or its port is taken), requests during the window get the failure back
// immediately instead of re-running port_command; the window doubles per
// consecutive failure, like the process supervisor's start backoff.
const (
	resolveBackoffBase = time.Second
	resolveBackoffMax  = 30 * time.Second
)

// entryState is the daemon's runtime record for one wildcard discovery
// entry: the entry, the shared listener it owns, and the resolver that
// materialises its worktrees on demand. Its dynamic projects are ordinary
// entries in the daemon's project table (flagged dynamic, with entry
// pointing here) so the control API sees them by name; they stay there —
// keeping the port port_command gave them — across idle stops, and leave
// only when their directory vanishes or the entry changes.
type entryState struct {
	entry    *config.Discovery
	bind     *binding
	resolver *resolver
	// maxRunning is the entry's max_running cap (0 = none), updated in
	// place by reloads.
	maxRunning atomic.Int64
}

func (d *Daemon) newEntryState(e *config.Discovery) *entryState {
	es := &entryState{entry: e}
	es.maxRunning.Store(int64(e.MaxRunning))
	es.resolver = newResolver(d, es)
	return es
}

// entryAddr returns the address a wildcard entry's listener binds.
func entryAddr(e *config.Discovery) string {
	return net.JoinHostPort(e.ListenHost(), strconv.Itoa(e.SupervisorPort()))
}

// sortedEntries returns the registered wildcard entries in name order.
func (d *Daemon) sortedEntries() []*entryState {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.sortedEntriesLocked()
}

func (d *Daemon) sortedEntriesLocked() []*entryState {
	names := make([]string, 0, len(d.entries))
	for name := range d.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*entryState, 0, len(names))
	for _, name := range names {
		out = append(out, d.entries[name])
	}
	return out
}

// bindEntry binds a wildcard entry's listener (routing unknown hosts to
// its resolver) and registers the entry. With serve set the listener
// starts serving at once; Run serves every listener together after
// binding them all.
func (d *Daemon) bindEntry(es *entryState, serve bool) error {
	addr := entryAddr(es.entry)
	b, err := d.listen(addr)
	if err != nil {
		return err
	}
	b.entry = es
	b.router.resolver = es.resolver
	es.bind = b
	d.mu.Lock()
	d.bindings[addr] = b
	d.entries[es.entry.Name] = es
	d.mu.Unlock()
	if serve {
		go d.serve(b)
	}
	d.logger.Printf("discovery %q: wildcard listener %s serves %s -> %s/<label>", es.entry.Name, addr, es.entry.URLPattern(), es.entry.Directory)
	return nil
}

// unbindEntry closes a wildcard entry's listener and forgets the entry.
// Its dynamic projects must have been retired first.
func (d *Daemon) unbindEntry(es *entryState) {
	d.mu.Lock()
	if d.bindings[es.bind.addr] == es.bind {
		delete(d.bindings, es.bind.addr)
	}
	if d.entries[es.entry.Name] == es {
		delete(d.entries, es.entry.Name)
	}
	d.mu.Unlock()
	d.unbind(es.bind)
}

// dynamicStates returns the entry's live dynamic projects, in name order.
func (d *Daemon) dynamicStatesLocked(es *entryState) []*projectState {
	var out []*projectState
	for _, st := range d.sortedStatesLocked() {
		if st.dynamic && st.entry == es {
			out = append(out, st)
		}
	}
	return out
}

// resolver materialises a wildcard entry's worktrees on demand: the first
// request for <label>.<base_domain> checks the candidate rules for
// <directory>/<label>, finds the application port (port_command, run in
// the worktree and cached for the daemon's lifetime, or the lowest free
// port of application_port_range), builds the project from the template,
// and registers it in the daemon's table and the listener's router. From
// then on the project is ordinary. Resolution runs at most once per label
// at a time; requests arriving meanwhile are held under the template's
// hold limits.
type resolver struct {
	d  *Daemon
	es *entryState
	// maxWait and maxHeld bound how long, and how many, requests wait for
	// a resolution in flight (the template's hold_max_wait_seconds and
	// hold_max_requests).
	maxWait time.Duration
	maxHeld int64
	held    atomic.Int64
	// terse is the entry template's diagnostic_logs: false. The resolver's
	// own 404 and 503 then name the label and nothing else — no worktree
	// paths, no port_command output, no other project's port — because a
	// wildcard listener behind nginx answers hostnames anyone on the
	// internet can guess. The full reason still reaches the daemon log.
	terse bool

	mu sync.Mutex
	// calls are the resolutions in flight, by label.
	calls map[string]*resolveCall
	// failures records labels whose last resolution failed with a 503, and
	// when the next attempt may run.
	failures map[string]*resolveFailure
}

// resolveCall is one resolution in flight; done is closed once handler or
// diag is set.
type resolveCall struct {
	done    chan struct{}
	handler http.Handler
	diag    *proxy.Diagnostic
	// detail is the untersed reason for the daemon log, set only when the
	// diagnostic the client gets had detail removed from it (see
	// resolver.terse). Empty means the diagnostic's own reason is the whole
	// story and the log line reads exactly as it always has.
	detail string
}

type resolveFailure struct {
	diag proxy.Diagnostic
	// detail is resolveCall.detail for the failure being remembered, so a
	// request served from the backoff window logs as loudly as the one that
	// discovered the failure.
	detail  string
	count   int
	retryAt time.Time
}

func newResolver(d *Daemon, es *entryState) *resolver {
	// A probe project carries the template's hold limits with defaults
	// applied, exactly as a materialised project will.
	p := es.entry.Project("probe", es.entry.Directory, 0)
	return &resolver{
		d:        d,
		es:       es,
		terse:    !p.DiagnosticLogsEnabled(),
		maxWait:  time.Duration(p.HoldMaxWaitSeconds) * time.Second,
		maxHeld:  int64(p.HoldMaxRequests),
		calls:    map[string]*resolveCall{},
		failures: map[string]*resolveFailure{},
	}
}

// serve handles a request on the entry's listener for a host no route
// claims yet: it resolves the label (or explains why it cannot) and, on
// success, hands the request to the new project's on-demand handler.
func (rs *resolver) serve(w http.ResponseWriter, r *http.Request, host string) {
	e := rs.es.entry
	label, ok := e.Label(host)
	if !ok {
		rs.d.logger.Printf("discovery %q: 404 for %s %s: host %q is not <label>.%s", e.Name, r.Method, r.URL.Path, host, e.BaseDomain)
		diag, _ := rs.public(&proxy.Diagnostic{
			Status: http.StatusNotFound,
			Title:  fmt.Sprintf("no project for host %q", host),
			Reason: fmt.Sprintf("The herd-wake listener on %s serves the git worktrees under %s at %s — exactly one label, directly under %s — and %q is not such a hostname.",
				rs.es.bind.addr, e.Directory, e.URLPattern(), e.BaseDomain, host),
			Hint: fmt.Sprintf("Open https://<worktree>.%s, where <worktree> is a directory name under %s.", e.BaseDomain, e.Directory),
		}, "", noWorktreeReason, noWorktreeHint)
		proxy.WriteDiagnostic(w, r, *diag)
		return
	}
	if rs.d.draining.Load() {
		proxy.WriteDiagnostic(w, r, proxy.Diagnostic{Status: http.StatusServiceUnavailable, Project: label, Reason: "The herd-wake daemon is shutting down.", Hint: " "})
		return
	}
	h, diag, detail := rs.resolve(r.Context(), label)
	if diag != nil {
		if detail == "" {
			detail = diag.Reason
		}
		rs.d.logger.Printf("discovery %q: %d for %s %s (%s): %s", e.Name, diag.Status, r.Method, r.URL.Path, host, detail)
		proxy.WriteDiagnostic(w, r, *diag)
		return
	}
	if h == nil {
		return // the client went away while waiting
	}
	h.ServeHTTP(w, r)
}

// resolve returns the handler for label, materialising the project if
// needed. Concurrent callers for the same label share one resolution —
// port_command runs once — and wait for its outcome (bounded by maxWait
// and the caller's context; a nil handler and nil diagnostic mean the
// caller gave up). A failed resolution is reported as a 503 diagnostic and
// not retried before its backoff elapses; a worktree that is not a
// candidate is a 404 and is re-checked on every request, so a re-created
// worktree materialises fresh.
func (rs *resolver) resolve(ctx context.Context, label string) (http.Handler, *proxy.Diagnostic, string) {
	if held := rs.held.Add(1); held > rs.maxHeld {
		rs.held.Add(-1)
		return nil, &proxy.Diagnostic{
			Status: http.StatusServiceUnavailable, Project: label, Hint: " ",
			Reason: fmt.Sprintf("Too many requests are already waiting for this worktree to be resolved (limit %d, hold_max_requests). Retry shortly.", rs.maxHeld),
		}, ""
	}
	defer rs.held.Add(-1)

	rs.mu.Lock()
	if c := rs.calls[label]; c != nil {
		rs.mu.Unlock()
		return rs.await(ctx, c)
	}
	if f := rs.failures[label]; f != nil && time.Now().Before(f.retryAt) {
		// Every field of the remembered failure is copied under the lock: a
		// concurrent materialise rewrites the same record when its own
		// resolution finishes, so reading it after the unlock would race.
		diag, retryAt, detail := f.diag, f.retryAt, f.detail
		rs.mu.Unlock()
		diag.Hint = fmt.Sprintf("Automatic retry in %s (the next request after that resolves the worktree again).", time.Until(retryAt).Round(100*time.Millisecond))
		return nil, &diag, detail
	}
	c := &resolveCall{done: make(chan struct{})}
	rs.calls[label] = c
	rs.mu.Unlock()

	c.handler, c.diag, c.detail = rs.materialise(label)

	rs.mu.Lock()
	delete(rs.calls, label)
	if c.diag == nil || c.diag.Status == http.StatusNotFound {
		delete(rs.failures, label)
	} else {
		f := rs.failures[label]
		if f == nil {
			f = &resolveFailure{}
			rs.failures[label] = f
		}
		f.count++
		f.retryAt = time.Now().Add(resolveBackoff(f.count))
		f.diag, f.detail = *c.diag, c.detail
	}
	rs.mu.Unlock()
	close(c.done)
	return c.handler, c.diag, c.detail
}

// await waits for a resolution another request started.
func (rs *resolver) await(ctx context.Context, c *resolveCall) (http.Handler, *proxy.Diagnostic, string) {
	wait := time.NewTimer(rs.maxWait)
	defer wait.Stop()
	select {
	case <-c.done:
		return c.handler, c.diag, c.detail
	case <-wait.C:
		return nil, &proxy.Diagnostic{
			Status: http.StatusServiceUnavailable, Hint: " ",
			Reason: fmt.Sprintf("Gave up waiting after %s (hold_max_wait_seconds) for the worktree to be resolved.", rs.maxWait),
		}, ""
	case <-ctx.Done():
		return nil, nil, ""
	}
}

// resolveBackoff returns the retry delay after n consecutive failures.
func resolveBackoff(n int) time.Duration {
	d := resolveBackoffBase
	for i := 1; i < n && d < resolveBackoffMax; i++ {
		d *= 2
	}
	return min(d, resolveBackoffMax)
}

// materialise builds and registers the project for label, or explains why
// it cannot: a 404 when <directory>/<label> is not a servable worktree, a
// 503 when its port cannot be determined or is taken.
func (rs *resolver) materialise(label string) (http.Handler, *proxy.Diagnostic, string) {
	d, e := rs.d, rs.es.entry
	dir, reason := discovery.Check(e, label)
	if reason != "" {
		diag, detail := rs.noWorktree(label, dir, reason)
		return nil, diag, detail
	}

	d.mu.RLock()
	existing := d.states[label]
	runCtx := d.runCtx
	d.mu.RUnlock()
	if existing != nil {
		diag, detail := rs.nameTaken(label, existing)
		return nil, diag, detail
	}

	port, err := rs.discoverPort(runCtx, label, dir)
	if err != nil {
		diag, detail := rs.public(&proxy.Diagnostic{
			Status: http.StatusServiceUnavailable, Project: label,
			Title:  fmt.Sprintf("worktree %q cannot be served", label),
			Reason: fmt.Sprintf("herd-wake could not determine the dev server port for %s: %v.", dir, err),
			Hint:   "Fix the worktree (or the discovery entry's port_command) and retry.",
		}, "", noPortReason, noPortHint)
		return nil, diag, detail
	}
	if owner := d.portOwner(port, label); owner != "" {
		diag, detail := rs.public(&proxy.Diagnostic{
			Status: http.StatusServiceUnavailable, Project: label,
			Title:  fmt.Sprintf("worktree %q cannot be served", label),
			Reason: fmt.Sprintf("The dev server port for %s is %d, which %s already uses; two servers cannot share it.", dir, port, owner),
			Hint:   "Give the worktree a port of its own (see the discovery entry's port_command / application_port_range) and retry.",
		}, "", noPortReason, noPortHint)
		return nil, diag, detail
	}

	p := e.Project(label, dir, port)
	st := d.newProjectState(p, idle.NewTracker())
	st.dynamic = true
	st.entry = rs.es
	st.sw = newHandlerSwitch(d.handler(st))
	route := &vanishGuard{dir: dir, next: st.sw, onVanish: func() {
		go d.dropDynamicByName(label, rs.es, "its worktree directory vanished")
	}, missing: func(w http.ResponseWriter, r *http.Request) {
		_, reason := discovery.Check(e, label)
		diag, detail := rs.noWorktree(label, dir, reason)
		if detail != "" {
			// A terse 404 keeps nothing of why on the page, so the daemon
			// log is the only place the operator can learn that the
			// worktree behind a live project has gone.
			d.logger.Printf("discovery %q: 404 for %s %s: %s", e.Name, r.Method, r.URL.Path, detail)
		}
		proxy.WriteDiagnostic(w, r, *diag)
	}}

	d.mu.Lock()
	if d.states[label] != nil {
		other := d.states[label]
		d.mu.Unlock()
		diag, detail := rs.nameTaken(label, other)
		return nil, diag, detail
	}
	d.states[label] = st
	st.bind = rs.es.bind
	rs.es.bind.members++
	d.mu.Unlock()
	rs.es.bind.router.setHost(p.Host, route)
	d.activate(st, runCtx)
	d.logger.Printf("discovery %q: materialised project %q for %s (%s)", e.Name, label, dir, p.PublicURL)
	d.logProxying(p)
	return route, nil, ""
}

// The wording a terse diagnostic says instead of the real reason: what
// went wrong, in terms a stranger on the internet learns nothing from, and
// where the operator reads the rest.
const (
	noWorktreeReason = "There is no servable worktree for this hostname."
	noWorktreeHint   = "Check the daemon log on the host for the rule that rejected it."
	noPortReason     = "herd-wake could not determine a usable dev server port for this worktree."
	noPortHint       = "See the daemon log on the host."
	nameTakenReason  = "A project of this name is already registered, so the worktree cannot be served through the wildcard listener."
	nameTakenHint    = "See the daemon log on the host."
)

// public returns the diagnostic to send to the client and the reason to
// log. In the default mode full is sent as it stands and there is no
// separate detail, so the daemon log reads exactly as it always has. Under
// terse mode the client gets title (when given), reason, and hint in place
// of full's — the status, and the label in Project, are all that survive —
// and full's reason becomes the log's detail, so turning the public pages
// terse costs the operator nothing.
func (rs *resolver) public(full *proxy.Diagnostic, title, reason, hint string) (*proxy.Diagnostic, string) {
	if !rs.terse {
		return full, ""
	}
	terse := *full
	if title != "" {
		terse.Title = title
	}
	terse.Reason, terse.Hint = reason, hint
	return &terse, full.Reason
}

// noWorktree is the 404 for a label whose directory fails the candidate
// rules, with the reason to log alongside it (see public).
func (rs *resolver) noWorktree(label, dir, reason string) (*proxy.Diagnostic, string) {
	e := rs.es.entry
	return rs.public(&proxy.Diagnostic{
		Status: http.StatusNotFound,
		Title:  fmt.Sprintf("no worktree named %q under %s", label, e.Directory),
		Reason: fmt.Sprintf("%s resolves to %s, which is not a worktree herd-wake can serve: %s.", e.PublicURL(label), dir, reason),
		Hint:   "Create the worktree (it is served on the next request), or check the discovery entry's rules: repository, require_files, exclude.",
	}, fmt.Sprintf("no worktree named %q", label), noWorktreeReason, noWorktreeHint)
}

// nameTaken is the 503 for a label whose name is already a registered
// project: a static one, or a dynamic one being taken out of service, with
// the reason to log alongside it (see public). The static case names the
// conflicting project's source file, which is a path on the box, so it goes
// through public like every other page the wildcard listener serves.
func (rs *resolver) nameTaken(label string, other *projectState) (*proxy.Diagnostic, string) {
	if other.dynamic && other.entry == rs.es {
		return rs.public(&proxy.Diagnostic{
			Status: http.StatusServiceUnavailable, Project: label,
			Reason: fmt.Sprintf("Worktree %q is being taken out of service (its directory vanished or it was reconfigured) and its process is still stopping.", label),
			Hint:   "Retry in a moment.",
		}, "", fmt.Sprintf("Worktree %q is being taken out of service; retry in a moment.", label), " ")
	}
	return rs.public(&proxy.Diagnostic{
		Status: http.StatusServiceUnavailable, Project: label,
		Title:  fmt.Sprintf("worktree %q cannot be served", label),
		Reason: fmt.Sprintf("A project named %q is already registered (from %s), so the worktree of that name cannot be served through the wildcard listener.", label, other.project.Source),
		Hint:   "Rename the worktree, or remove the conflicting project.",
	}, fmt.Sprintf("worktree %q cannot be served", label), nameTakenReason, nameTakenHint)
}

// discoverPort finds the application port for a label being materialised:
// port_command, run in the worktree (bounded by the discovery timeout, and
// by ctx so a daemon shutdown ends it), or the lowest port of
// application_port_range nothing live uses. The project keeps the port
// until project:restart runs port_command again or the project is dropped.
func (rs *resolver) discoverPort(ctx context.Context, label, dir string) (int, error) {
	e := rs.es.entry
	if e.PortCommand != "" {
		if ctx == nil {
			ctx = context.Background()
		}
		port, err := discovery.RunPortCommand(ctx, dir, e.PortCommand, &e.Template, discovery.DefaultPortCommandTimeout)
		if err != nil {
			return 0, fmt.Errorf("port_command %w", err)
		}
		return port, nil
	}
	rng := e.ApplicationPortRange
	for port := rng[0]; port <= rng[1]; port++ {
		if rs.d.portOwner(port, label) == "" {
			return port, nil
		}
	}
	return 0, fmt.Errorf("application_port_range %v is exhausted", rng)
}

// portOwner reports who holds port among live projects (except the one
// named except) and wildcard listeners; empty when it is free.
func (d *Daemon) portOwner(port int, except string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for name, st := range d.states {
		if name == except {
			continue
		}
		switch port {
		case st.project.SupervisorPort:
			return fmt.Sprintf("project %q (supervisor_port)", name)
		case st.project.ApplicationPort:
			return fmt.Sprintf("project %q (application_port)", name)
		}
	}
	for name, es := range d.entries {
		if es.entry.SupervisorPort() == port {
			return fmt.Sprintf("wildcard entry %q (supervisor_port)", name)
		}
	}
	return ""
}

// vanishGuard fronts a dynamic project's handler: a request for a
// worktree whose directory is gone gets the 404 the resolver would give and
// triggers the project's removal (stopping its process gracefully) instead
// of being forwarded. The stat is cheap next to a proxied request.
type vanishGuard struct {
	dir      string
	next     http.Handler
	once     sync.Once
	onVanish func()
	missing  http.HandlerFunc
}

func (g *vanishGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, err := os.Stat(g.dir); err != nil {
		g.once.Do(g.onVanish)
		g.missing(w, r)
		return
	}
	g.next.ServeHTTP(w, r)
}

// vanishAware is the idle monitor's view of a dynamic project: after an
// idle stop it checks whether the worktree directory still exists and
// drops the project when it does not.
type vanishAware struct {
	idle.Process
	after func()
}

func (p vanishAware) Stop(ctx context.Context) error {
	err := p.Process.Stop(ctx)
	p.after()
	return err
}

// dropIfVanished drops a stopped dynamic project whose worktree directory
// no longer exists.
func (d *Daemon) dropIfVanished(st *projectState) {
	if _, err := os.Stat(st.project.WorkingDirectory); err != nil {
		d.dropDynamic(st, "its worktree directory is gone")
	}
}

// dropDynamicByName drops the entry's dynamic project currently registered
// under label, if any.
func (d *Daemon) dropDynamicByName(label string, es *entryState, why string) {
	d.mu.RLock()
	st := d.states[label]
	d.mu.RUnlock()
	if st != nil && st.dynamic && st.entry == es {
		d.dropDynamic(st, why)
	}
}

// dropDynamic takes a dynamic project out of service: its route is removed
// (the next request for its host resolves the label afresh), its monitor
// ended, its supervisor retired and process stopped gracefully, and — once
// the process is gone — its table entry removed. It is idempotent and safe
// against a reload retiring the same project.
func (d *Daemon) dropDynamic(st *projectState, why string) {
	d.logger.Printf("project %q: dropping dynamic project: %s", st.project.Name, why)
	ctx, cancel := context.WithTimeout(context.Background(), retireBound([]retiring{{st: st}}))
	defer cancel()
	d.retire(ctx, retiring{st: st})
	d.mu.Lock()
	if d.states[st.project.Name] == st {
		delete(d.states, st.project.Name)
	}
	d.mu.Unlock()
}

// restartDynamic is project:restart for a dynamic project: it re-checks
// the worktree, re-runs port_command, and — when the port changed —
// replaces the project with one on the new port (like a reload swapping a
// changed project: the old process is stopped behind a suspended switch,
// the tracker survives) before starting it.
func (d *Daemon) restartDynamic(ctx context.Context, st *projectState) (control.ProjectStatus, error) {
	es := st.entry
	e := es.entry
	label := st.project.Name

	dir, reason := discovery.Check(e, label)
	if reason != "" {
		d.dropDynamic(st, "its worktree is no longer servable: "+reason)
		return control.ProjectStatus{}, fmt.Errorf("project %q: %s is no longer a servable worktree (%s); the project has been dropped", label, dir, reason)
	}
	port := st.project.ApplicationPort
	if e.PortCommand != "" {
		p, err := discovery.RunPortCommand(ctx, dir, e.PortCommand, &e.Template, discovery.DefaultPortCommandTimeout)
		if err != nil {
			return control.ProjectStatus{}, fmt.Errorf("project %q: port_command %v; not restarted", label, err)
		}
		port = p
	}
	if port == st.project.ApplicationPort {
		if err := d.restart(ctx, st); err != nil {
			return control.ProjectStatus{}, err
		}
		return d.projectStatus(st), nil
	}
	if owner := d.portOwner(port, label); owner != "" {
		return control.ProjectStatus{}, fmt.Errorf("project %q: port_command now prints port %d, which %s already uses; not restarted", label, port, owner)
	}

	// The port changed: swap the project like a reload does.
	d.reloadMu.Lock()
	defer d.reloadMu.Unlock()
	d.mu.Lock()
	if d.states[label] != st {
		d.mu.Unlock()
		return control.ProjectStatus{}, fmt.Errorf("project %q was dropped or replaced meanwhile; retry", label)
	}
	runCtx := d.runCtx
	st.sw.Suspend()
	d.mu.Unlock()
	rctx, cancel := context.WithTimeout(context.Background(), retireBound([]retiring{{st: st}}))
	defer cancel()
	d.retire(rctx, retiring{st: st, keepBinding: true})

	next := d.newProjectState(e.Project(label, dir, port), st.tracker)
	next.dynamic, next.entry = true, es
	if err := d.commission(next, st, runCtx); err != nil {
		return control.ProjectStatus{}, err // unreachable: an inherited route never fails
	}
	d.logger.Printf("project %q: application port changed %d -> %d on restart", label, st.project.ApplicationPort, port)
	if err := d.startManual(ctx, next); err != nil {
		return control.ProjectStatus{}, err
	}
	return d.projectStatus(next), nil
}

// wildcardStatus renders one entry's control-API status.
func (d *Daemon) wildcardStatusLocked(es *entryState) control.WildcardStatus {
	states := d.dynamicStatesLocked(es)
	running, _ := countRunning(states, nil)
	return control.WildcardStatus{
		Name:           es.entry.Name,
		BaseDomain:     es.entry.BaseDomain,
		URLPattern:     es.entry.URLPattern(),
		Directory:      es.entry.Directory,
		SupervisorPort: es.entry.SupervisorPort(),
		Projects:       len(states),
		Running:        running,
		MaxRunning:     int(es.maxRunning.Load()),
		NextEviction:   nextEviction(states),
	}
}
