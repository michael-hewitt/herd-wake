package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/idle"
)

// Reload implements control.Provider: it re-reads the config file (and its
// projects.d directory), validates it, and applies the difference to the
// running project set. Reloads are serialized, and a reload runs to
// completion even if the caller goes away.
//
// An invalid config rejects the whole reload: the validation errors come
// back in the response and nothing changes. Otherwise projects are diffed
// by name against the live set:
//
//   - added: a supervisor, tracker, idle monitor, and route are created
//     (binding the port if no listener exists there yet); always_on
//     projects start immediately. A bind failure is reported in Errors and
//     does not abort the rest.
//   - removed: the route is removed first (closing the listener when
//     nothing else is on it), then the process group is stopped
//     gracefully, and the monitor ends.
//   - changed (any field differs): the project is retired — its process
//     stopped if starting or running — and replaced by a fresh supervisor
//     for the new config (backoff reset), keeping the activity tracker so
//     leases survive. The route is kept when the address and host are
//     unchanged (requests arriving meanwhile wait for the swap) and
//     re-created otherwise. always_on projects start immediately; anything
//     else cold-starts on the next request. If rebinding fails, the
//     previous registration is restored (and the error reported) so the
//     port keeps serving.
//   - unchanged: untouched — running processes, idle timers, leases, and
//     in-flight requests are unaffected.
//
// Wildcard discovery entries are diffed the same way: an unchanged entry
// keeps its listener and every dynamic project it materialised; a changed
// or removed entry has its dynamic projects stopped and dropped (they are
// listed under Removed) and its listener closed, then — if it still
// exists — rebuilt; an added entry binds its listener.
func (d *Daemon) Reload(_ context.Context) (control.ReloadResponse, error) {
	d.reloadMu.Lock()
	defer d.reloadMu.Unlock()

	resp := control.ReloadResponse{
		ConfigPath: d.configPath,
		Added:      []string{},
		Removed:    []string{},
		Changed:    []string{},
		Unchanged:  []string{},
	}
	if d.configPath == "" {
		return resp, errors.New("reload unavailable: the daemon was started from an in-memory configuration, not a config file")
	}
	if d.draining.Load() {
		return resp, errors.New("the herd-wake daemon is shutting down")
	}
	d.mu.RLock()
	runCtx := d.runCtx
	current := maps.Clone(d.states)
	currentEntries := maps.Clone(d.entries)
	d.mu.RUnlock()
	if runCtx == nil {
		return resp, errors.New("the herd-wake daemon is not running")
	}

	cfg, err := config.Load(d.configPath)
	if err != nil {
		resp.Errors = errorLines(err)
		d.logger.Printf("reload rejected: config %s is invalid:\n  - %s", d.configPath, strings.Join(resp.Errors, "\n  - "))
		return resp, nil
	}

	// Diff projects by name. Dynamic projects are not in the config; they
	// belong to their entry and are diffed with it below — unless a static
	// project now claims the same name, which takes it over as a change.
	for _, name := range cfg.ProjectNames() {
		st, ok := current[name]
		switch {
		case !ok:
			resp.Added = append(resp.Added, name)
		case !st.dynamic && st.project.Equivalent(cfg.Projects[name]):
			resp.Unchanged = append(resp.Unchanged, name)
		default:
			resp.Changed = append(resp.Changed, name)
		}
	}
	for name, st := range current {
		if _, ok := cfg.Projects[name]; !ok && !st.dynamic {
			resp.Removed = append(resp.Removed, name)
		}
	}

	// Diff wildcard entries by name.
	wanted := map[string]*config.Discovery{}
	for _, e := range cfg.Discovery {
		if e.Wildcard() {
			wanted[e.Name] = e
		}
	}
	var entriesAdded, entriesRemoved, entriesChanged, entriesUnchanged []string
	for name, es := range currentEntries {
		switch e, ok := wanted[name]; {
		case !ok:
			entriesRemoved = append(entriesRemoved, name)
		case es.entry.Equivalent(e):
			entriesUnchanged = append(entriesUnchanged, name)
		default:
			entriesChanged = append(entriesChanged, name)
		}
	}
	for name := range wanted {
		if _, ok := currentEntries[name]; !ok {
			entriesAdded = append(entriesAdded, name)
		}
	}
	if len(wanted) > 0 || len(currentEntries) > 0 {
		sort.Strings(entriesAdded)
		sort.Strings(entriesRemoved)
		sort.Strings(entriesChanged)
		sort.Strings(entriesUnchanged)
		resp.Wildcards = &control.ReloadDiff{
			Added: orEmpty(entriesAdded), Removed: orEmpty(entriesRemoved),
			Changed: orEmpty(entriesChanged), Unchanged: orEmpty(entriesUnchanged),
		}
	}

	// Detach: removed projects leave the table now (they are gone as far as
	// the control API is concerned); a changed project on an unchanged
	// route keeps its table entry and route but stops routing requests
	// until its replacement is installed; the dynamic projects of changed
	// or removed entries leave the table like removed projects.
	var retire []retiring
	var retiredEntries []*entryState
	d.mu.Lock()
	for _, name := range resp.Removed {
		st := d.states[name]
		delete(d.states, name)
		retire = append(retire, retiring{st: st})
	}
	for _, name := range resp.Changed {
		st := d.states[name]
		keep := !st.dynamic && sameRoute(st.project, cfg.Projects[name])
		if keep {
			st.sw.Suspend()
		}
		retire = append(retire, retiring{st: st, keepBinding: keep})
	}
	for _, name := range append(append([]string{}, entriesRemoved...), entriesChanged...) {
		es := currentEntries[name]
		for _, st := range d.dynamicStatesLocked(es) {
			if d.states[st.project.Name] != st || contains(resp.Changed, st.project.Name) {
				continue // taken over by a static project above
			}
			delete(d.states, st.project.Name)
			resp.Removed = append(resp.Removed, st.project.Name)
			retire = append(retire, retiring{st: st})
		}
		retiredEntries = append(retiredEntries, es)
	}
	d.mu.Unlock()
	sort.Strings(resp.Removed)

	// Tear down (outside the table lock: unaffected projects keep serving
	// and answering the control API meanwhile). This waits for every
	// retired process group to be gone, so a replacement can never race
	// its predecessor for the application port.
	d.retireAll(retire)
	for _, es := range retiredEntries {
		d.unbindEntry(es)
	}

	// Rebuild. Removed ports were released above, so an added project may
	// take over a port a removed one held.
	for _, name := range resp.Added {
		st := d.newProjectState(cfg.Projects[name], idle.NewTracker())
		if err := d.commission(st, nil, runCtx); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("project %q: %v", name, err))
		}
	}
	for _, name := range resp.Changed {
		old := current[name]
		var inherited *projectState
		if !old.dynamic && sameRoute(old.project, cfg.Projects[name]) {
			inherited = old
		}
		st := d.newProjectState(cfg.Projects[name], old.tracker)
		err := d.commission(st, inherited, runCtx)
		if err == nil {
			continue
		}
		resp.Errors = append(resp.Errors, fmt.Sprintf("project %q: %v; keeping its previous registration", name, err))
		// The new address could not be bound: restore the previous
		// registration so the project keeps serving on its old port (it is
		// stopped now, and cold-starts on the next request). The next
		// reload sees it as changed again and retries.
		prev := d.newProjectState(old.project, old.tracker)
		if err := d.commission(prev, nil, runCtx); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf(
				"project %q: could not restore its previous listener either (%v); it is unregistered until the next successful reload", name, err))
			d.mu.Lock()
			delete(d.states, name)
			d.mu.Unlock()
		}
	}
	for _, name := range append(append([]string{}, entriesAdded...), entriesChanged...) {
		es := d.newEntryState(wanted[name])
		if err := d.bindEntry(es, true); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("discovery %q: %v; its worktrees are unavailable until the next successful reload", name, err))
		}
	}

	now := time.Now()
	d.mu.Lock()
	d.lastReloadAt = now
	d.mu.Unlock()
	resp.Applied = true
	resp.ReloadedAt = now
	return resp, nil
}

// commission puts st into service: it builds the project's handler, takes
// over the route of the project it replaces (inherited, on the same
// address and host) or attaches a fresh route (binding the listener if
// needed), starts its monitor (or always_on start), and logs it.
func (d *Daemon) commission(st *projectState, inherited *projectState, runCtx context.Context) error {
	h := d.handler(st)
	if inherited != nil {
		st.sw = inherited.sw
		st.bind = inherited.bind
		d.mu.Lock()
		d.states[st.project.Name] = st
		d.mu.Unlock()
		st.sw.Install(h)
	} else {
		st.sw = newHandlerSwitch(h)
		if err := d.attach(st, true); err != nil {
			return err
		}
	}
	d.activate(st, runCtx)
	d.logProxying(st.project)
	return nil
}

// sameRoute reports whether two configurations of a project bind the same
// supervisor address and host, so the route can be kept across the change.
func sameRoute(a, b *config.Project) bool {
	return listenAddr(a) == listenAddr(b) && a.Host == b.Host
}

// logReload writes a reload's outcome to the daemon log.
func (d *Daemon) logReload(resp control.ReloadResponse) {
	if !resp.Applied {
		return // the rejection was logged when it happened
	}
	d.logger.Printf("config reloaded: %d added %v, %d removed %v, %d changed %v, %d unchanged",
		len(resp.Added), resp.Added, len(resp.Removed), resp.Removed, len(resp.Changed), resp.Changed, len(resp.Unchanged))
	if w := resp.Wildcards; w != nil {
		d.logger.Printf("wildcard entries: %d added %v, %d removed %v, %d changed %v, %d unchanged",
			len(w.Added), w.Added, len(w.Removed), w.Removed, len(w.Changed), w.Changed, len(w.Unchanged))
	}
	for _, e := range resp.Errors {
		d.logger.Printf("reload: %s", e)
	}
}

// errorLines splits a (possibly joined, possibly multi-line) error into
// one message per line, for reporting.
func errorLines(err error) []string {
	var lines []string
	for line := range strings.SplitSeq(err.Error(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
