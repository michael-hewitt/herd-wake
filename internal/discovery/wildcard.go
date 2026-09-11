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
// domain of) a wildcard entry and syncing again removes the proxy too.
const StateFileName = "sync-state.yaml"

// StatePath returns the sync state file next to the config at configPath.
func StatePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), StateFileName)
}

// syncState is the YAML shape of the state file.
type syncState struct {
	// Wildcards maps a wildcard entry's name to the Herd proxy sync
	// registered for it.
	Wildcards map[string]wildcardState `yaml:"wildcards,omitempty"`
}

// wildcardState is one registered wildcard proxy.
type wildcardState struct {
	BaseDomain     string `yaml:"base_domain"`
	SupervisorPort int    `yaml:"supervisor_port"`
}

// loadState reads the state file; a missing file is an empty state.
func loadState(path string) (*syncState, error) {
	st := &syncState{Wildcards: map[string]wildcardState{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sync state %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parse sync state %s: %w", path, err)
	}
	if st.Wildcards == nil {
		st.Wildcards = map[string]wildcardState{}
	}
	return st, nil
}

// saveState writes the state file (removing it when there is nothing to
// record).
func saveState(path string, st *syncState) error {
	if len(st.Wildcards) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	var buf bytes.Buffer
	buf.WriteString("# Written by herd-wake sync: the wildcard Herd proxies it registered, so a\n# removed wildcard entry is unproxied on the next sync. Do not edit.\n")
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
func (s *syncer) syncWildcard(d *config.Discovery) *EntryResult {
	res := &EntryResult{
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
	return res
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
