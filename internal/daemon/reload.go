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
//   - added: a supervisor, tracker, idle monitor, and listener are created
//     and the port bound; always_on projects start immediately. A bind
//     failure is reported in Errors and does not abort the rest.
//   - removed: the listener is closed first, then the process group is
//     stopped gracefully, and the monitor ends.
//   - changed (any field differs): the project is retired — its process
//     stopped if starting or running — and replaced by a fresh supervisor
//     for the new config (backoff reset), keeping the activity tracker so
//     leases survive. The listener is kept when the address is unchanged
//     (requests arriving meanwhile wait for the swap) and rebound otherwise.
//     always_on projects start immediately; anything else cold-starts on
//     the next request. If rebinding fails, the previous registration is
//     restored (and the error reported) so the port keeps serving.
//   - unchanged: untouched — running processes, idle timers, leases, and
//     in-flight requests are unaffected.
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

	// Diff by name.
	for _, name := range cfg.ProjectNames() {
		st, ok := current[name]
		switch {
		case !ok:
			resp.Added = append(resp.Added, name)
		case st.project.Equivalent(cfg.Projects[name]):
			resp.Unchanged = append(resp.Unchanged, name)
		default:
			resp.Changed = append(resp.Changed, name)
		}
	}
	for name := range current {
		if _, ok := cfg.Projects[name]; !ok {
			resp.Removed = append(resp.Removed, name)
		}
	}
	sort.Strings(resp.Removed)

	// Detach: removed projects leave the table now (they are gone as far as
	// the control API is concerned); a changed project on an unchanged
	// address keeps its table entry and listener but stops routing
	// requests until its replacement is installed.
	var retire []retiring
	d.mu.Lock()
	for _, name := range resp.Removed {
		st := d.states[name]
		delete(d.states, name)
		retire = append(retire, retiring{st: st})
	}
	for _, name := range resp.Changed {
		st := d.states[name]
		keep := sameListener(st.project, cfg.Projects[name])
		if keep {
			st.bind.sw.Suspend()
		}
		retire = append(retire, retiring{st: st, keepBinding: keep})
	}
	d.mu.Unlock()

	// Tear down (outside the table lock: unaffected projects keep serving
	// and answering the control API meanwhile). This waits for every
	// retired process group to be gone, so a replacement can never race
	// its predecessor for the application port.
	d.retireAll(retire)

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
		var inherited *binding
		if sameListener(old.project, cfg.Projects[name]) {
			inherited = old.bind
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

	now := time.Now()
	d.mu.Lock()
	d.lastReloadAt = now
	d.mu.Unlock()
	resp.Applied = true
	resp.ReloadedAt = now
	return resp, nil
}

// commission puts st into service: it binds the project's listener (unless
// one is inherited from the project it replaces), registers st in the
// table, routes the listener to st's handler, starts its monitor (or
// always_on start), and begins serving a fresh listener.
func (d *Daemon) commission(st *projectState, inherited *binding, runCtx context.Context) error {
	h := d.handler(st)
	if inherited != nil {
		st.bind = inherited
	} else {
		b, err := d.bindProject(st.project, h)
		if err != nil {
			return err
		}
		st.bind = b
	}

	d.mu.Lock()
	d.states[st.project.Name] = st
	if inherited != nil {
		inherited.sw.Install(h)
	}
	d.mu.Unlock()

	d.activate(st, runCtx)
	if inherited == nil {
		go d.serve(st.bind)
	}
	d.logProxying(st.project)
	return nil
}

// sameListener reports whether two configurations of a project bind the
// same supervisor address, so the listener can be kept across the change.
func sameListener(a, b *config.Project) bool {
	return listenAddr(a) == listenAddr(b)
}

// logReload writes a reload's outcome to the daemon log.
func (d *Daemon) logReload(resp control.ReloadResponse) {
	if !resp.Applied {
		return // the rejection was logged when it happened
	}
	d.logger.Printf("config reloaded: %d added %v, %d removed %v, %d changed %v, %d unchanged",
		len(resp.Added), resp.Added, len(resp.Removed), resp.Removed, len(resp.Changed), resp.Changed, len(resp.Unchanged))
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
