package discovery

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/herd"
	"gopkg.in/yaml.v3"
)

// StateFileName is the file next to the config in which sync records the
// wildcard Herd proxies it created, so that removing (or renaming the
// domain of) a wildcard entry and syncing again removes the proxy too,
// and the bring-your-own-proxy notes it has already shown.
const StateFileName = "sync-state.yaml"

// StatePath returns the sync state file next to the config at configPath.
func StatePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), StateFileName)
}

// syncState is the YAML shape of the state file. Older files carry only
// wildcards; every key is optional so they still load.
type syncState struct {
	// Wildcards maps a wildcard entry's name to the Herd proxy sync
	// registered for it.
	Wildcards map[string]wildcardState `yaml:"wildcards,omitempty"`
	// ProxyNotes maps a wildcard entry's name to the listener the
	// bring-your-own-proxy note was last shown for, so the note is printed
	// once and again only when base_domain or supervisor_port changes.
	ProxyNotes map[string]wildcardState `yaml:"proxy_notes,omitempty"`
}

// wildcardState is one registered wildcard proxy (or the listener a proxy
// note was shown for).
type wildcardState struct {
	BaseDomain     string `yaml:"base_domain"`
	SupervisorPort int    `yaml:"supervisor_port"`
}

// empty reports whether there is nothing worth keeping a file for.
func (st *syncState) empty() bool {
	return len(st.Wildcards) == 0 && len(st.ProxyNotes) == 0
}

// loadState reads the state file; a missing file is an empty state.
func loadState(path string) (*syncState, error) {
	st := &syncState{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read sync state %s: %w", path, err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, st); err != nil {
			return nil, fmt.Errorf("parse sync state %s: %w", path, err)
		}
	}
	if st.Wildcards == nil {
		st.Wildcards = map[string]wildcardState{}
	}
	if st.ProxyNotes == nil {
		st.ProxyNotes = map[string]wildcardState{}
	}
	return st, nil
}

// saveState writes the state file (removing it when there is nothing to
// record).
func saveState(path string, st *syncState) error {
	if st.empty() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	var buf bytes.Buffer
	buf.WriteString("# Written by herd-wake sync: the wildcard Herd proxies it registered, so a\n# removed wildcard entry is unproxied on the next sync, and the proxy notes\n# it has shown for entries synced without a Herd CLI. Do not edit.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(st); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return writeAtomic(path, buf.Bytes())
}

// syncWildcard reconciles one wildcard entry with Herd: it makes sure the
// single proxy `herd proxy <base name> http://127.0.0.1:<supervisor_port>
// --secure` exists (Herd's site file and certificate both cover
// *.<base_domain>), refusing a base name that would shadow a PHP site, and
// writes no projects.d file — worktrees are resolved by the daemon on
// demand. A managed file left over from proxy mode is reported, never
// deleted.
//
// Without a Herd CLI the entry is left to the user's own proxy instead,
// and note (empty when nothing is to be said) tells them what to point it
// at — once, until the listener changes.
func (s *syncer) syncWildcard(d *config.Discovery) (res *EntryResult, note string) {
	res = &EntryResult{
		Name:           d.Name,
		Mode:           config.ModeWildcard,
		Directory:      d.Directory,
		BaseDomain:     d.BaseDomain,
		SupervisorPort: d.SupervisorPort(),
		URLPattern:     d.URLPattern(),
		Added:          []ProjectSummary{},
		Updated:        []ProjectSummary{},
		Unchanged:      []ProjectSummary{},
		Removed:        []ProjectSummary{},
		Skipped:        []Skip{},
		Herd:           []HerdAction{},
	}

	if stale := d.ManagedPath(s.cfg.Path); fileExists(stale) {
		res.Notices = append(res.Notices, fmt.Sprintf(
			"%s exists (left over from proxy mode) and its projects are still registered; delete it, run `herd unproxy <worktree>` for each of them, and reload once you no longer need them",
			stale))
	}

	note = s.proxyNote(d)
	if s.herdOff(d) {
		return res, note
	}

	host := d.BaseDomain
	site, _ := herd.SiteName(host)
	a := HerdAction{Action: "proxy", Site: site, Project: d.Name, Port: d.SupervisorPort(), Command: herd.ProxyCommand(site, d.SupervisorPort())}
	switch {
	case !d.HerdEnabled():
		a.Status, a.Detail = HerdManual, "herd: false for this discovery entry"
	case s.opts.Herd == nil:
		a.Status, a.Detail = HerdManual, s.opts.HerdNote
	default:
		port, exists, err := s.opts.Herd.ProxyTarget(host)
		switch {
		case err != nil:
			a.Status, a.Detail = HerdManual, err.Error()
		case exists && port == d.SupervisorPort():
			a.Status, a.Detail = HerdSkipped, fmt.Sprintf("already proxied to 127.0.0.1:%d", port)
		case exists && port == 0:
			a.Status, a.Detail = HerdRefused, fmt.Sprintf("Herd already has a site file for %s that is not a proxy (%s); it looks like a secured PHP site", host, s.opts.Herd.SiteFile(host))
			res.Errors = append(res.Errors, "refusing to claim "+host+": "+a.Detail)
		default:
			if exists {
				a.Detail = fmt.Sprintf("re-pointed from 127.0.0.1:%d", port)
			} else if reason, err := s.opts.Herd.Conflict(s.ctx, site); err != nil {
				// Never claim a Herd site that already belongs to something
				// else (the collision safeguard, on the base name).
				a.Status, a.Detail = HerdManual, fmt.Sprintf("could not check Herd for a conflicting site: %v", err)
			} else if reason != "" {
				a.Status, a.Detail = HerdRefused, reason
				res.Errors = append(res.Errors, "refusing to claim "+host+": "+reason)
			}
			if a.Status == "" {
				s.runHerd(&a, func() error { return s.opts.Herd.Proxy(s.ctx, site, d.SupervisorPort()) })
			}
		}
	}
	res.Herd = append(res.Herd, a)

	// Remember the registration so a later sync can undo it when the entry
	// disappears; a proxy that is (or already was) in place is recorded.
	if a.Status == HerdDone || a.Status == HerdSkipped || a.Status == HerdDryRun {
		s.state.Wildcards[d.Name] = wildcardState{BaseDomain: d.BaseDomain, SupervisorPort: d.SupervisorPort()}
	}
	return res, note
}

// proxyNote returns the bring-your-own-proxy note for a wildcard entry
// synced without a Herd CLI (`herd:` unset or true), or "" when there is
// nothing to say: Herd is in use, --no-herd asked for Herd commands
// instead, `herd: false`, or the same note was already shown for this
// listener. Showing it records it in the state (which a dry run never
// writes), so the next sync is silent until base_domain or the port
// changes. A sync that uses Herd (or was told to leave it alone) forgets
// the record, so the note comes back should Herd go away again.
func (s *syncer) proxyNote(d *config.Discovery) string {
	if s.opts.Herd != nil || !s.opts.HerdUnavailable || !d.HerdEnabled() {
		delete(s.state.ProxyNotes, d.Name)
		return ""
	}
	listener := wildcardState{BaseDomain: d.BaseDomain, SupervisorPort: d.SupervisorPort()}
	if s.state.ProxyNotes[d.Name] == listener {
		return ""
	}
	s.state.ProxyNotes[d.Name] = listener
	note := fmt.Sprintf("no Herd CLI found; point your proxy at herd-wake yourself for discovery %q: *.%s → http://127.0.0.1:%d (see README \"Bring your own proxy\" for the nginx and Caddy blocks)",
		d.Name, d.BaseDomain, d.SupervisorPort())
	if s.opts.GOOS == "darwin" {
		site, _ := herd.SiteName(d.BaseDomain)
		note += fmt.Sprintf("; or install Laravel Herd and run: %s", herd.ProxyCommand(site, d.SupervisorPort()))
	}
	return note
}

// retireWildcards unproxies the registrations recorded before this sync
// (previous) whose entry is gone from the config or whose base_domain
// changed, returning one result per retired registration. The current
// state keeps a registration whose proxy could not be removed, so the next
// sync retries.
func (s *syncer) retireWildcards(previous map[string]wildcardState) []*EntryResult {
	live := map[string]*config.Discovery{}
	for _, d := range s.cfg.Discovery {
		if d.Wildcard() {
			live[d.Name] = d
		}
	}
	// A removed wildcard entry takes its proxy note with it (a changed
	// listener was re-noted by syncWildcard).
	for name := range s.state.ProxyNotes {
		if _, ok := live[name]; !ok {
			delete(s.state.ProxyNotes, name)
		}
	}
	names := make([]string, 0, len(previous))
	for name := range previous {
		names = append(names, name)
	}
	sort.Strings(names)

	var results []*EntryResult
	for _, name := range names {
		prev := previous[name]
		if d, ok := live[name]; ok && d.BaseDomain == prev.BaseDomain {
			continue
		}
		res := &EntryResult{
			Name:           name,
			Mode:           config.ModeWildcard,
			Retired:        true,
			BaseDomain:     prev.BaseDomain,
			SupervisorPort: prev.SupervisorPort,
			URLPattern:     "https://" + config.LabelPlaceholder + "." + prev.BaseDomain,
			Added:          []ProjectSummary{},
			Updated:        []ProjectSummary{},
			Unchanged:      []ProjectSummary{},
			Removed:        []ProjectSummary{},
			Skipped:        []Skip{},
			Herd:           []HerdAction{},
		}
		site, _ := herd.SiteName(prev.BaseDomain)
		a := s.unproxyAction(true, &config.Project{Name: name, PublicURL: "https://" + prev.BaseDomain, SupervisorPort: prev.SupervisorPort})
		a.Site, a.Project = site, name
		res.Herd = append(res.Herd, a)
		// A dry run (or a proxy that could not be removed) keeps the old
		// registration recorded for the next sync; a re-domained entry's
		// new registration was recorded by syncWildcard and stays.
		if (a.Status == HerdDone || a.Status == HerdSkipped) && s.state.Wildcards[name] == prev {
			delete(s.state.Wildcards, name)
		}
		results = append(results, res)
	}
	return results
}

// fileExists reports whether a regular file exists at path.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
