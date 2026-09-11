// Package discovery implements `herd-wake sync` and `herd-wake
// project:remove`, plus the candidate rules the daemon applies when it
// resolves a worktree on demand (Check).
//
// For a proxy-mode discovery entry, sync scans the entry's directory for
// git worktrees, keeps one generated project per worktree in a managed
// projects.d file, allocates stable ports, and creates or removes the
// matching Herd proxy entries. For a wildcard entry it does exactly one
// thing: ensure the single Herd proxy for the entry's base domain exists
// (recording it in a small state file so a removed entry is unproxied
// later); worktrees are resolved by the daemon from the request's Host.
//
// The user's hand-written config.yaml is never rewritten; every generated
// project lives in projects.d/<discovery name>.yaml, which carries a
// header marking it as managed. Files without that header are never
// overwritten.
package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/herd"
	"gopkg.in/yaml.v3"
)

// DefaultPortCommandTimeout bounds one port_command run.
const DefaultPortCommandTimeout = 15 * time.Second

// Options configures Sync and Remove.
type Options struct {
	// Herd is the Herd CLI to use for proxy management, or nil when it is
	// unavailable or disabled — then no Herd command runs and the manual
	// commands are reported instead. HerdNote says why (shown to the
	// user).
	Herd     *herd.CLI
	HerdNote string
	// DryRun computes everything but writes no file and runs no Herd
	// command that changes anything (`herd paths`/`herd links` still run).
	DryRun bool
	// PortCommandTimeout bounds each port_command run (default
	// DefaultPortCommandTimeout).
	PortCommandTimeout time.Duration
}

// Herd action outcomes.
const (
	// HerdDone: the command ran successfully.
	HerdDone = "done"
	// HerdSkipped: nothing to do (already proxied, no site file, ...).
	HerdSkipped = "skipped"
	// HerdDryRun: the command would have run.
	HerdDryRun = "dry-run"
	// HerdManual: the command could not be run (Herd unavailable or
	// disabled, or it failed); the user should run Command by hand.
	HerdManual = "manual"
	// HerdRefused: the name would shadow an existing Herd site (a parked
	// or linked PHP site, or a non-proxy site file), so nothing was done.
	HerdRefused = "refused"
)

// HerdAction is one Herd proxy change sync wanted to make.
type HerdAction struct {
	// Action is "proxy" or "unproxy".
	Action string `json:"action"`
	// Site is the Herd site name (the project's host without its TLD).
	Site    string `json:"site"`
	Project string `json:"project"`
	Port    int    `json:"port,omitempty"`
	// Command is the equivalent shell command.
	Command string `json:"command"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// ProjectSummary identifies one generated project in a result.
type ProjectSummary struct {
	Name            string `json:"name"`
	Directory       string `json:"directory"`
	PublicURL       string `json:"public_url"`
	SupervisorPort  int    `json:"supervisor_port"`
	ApplicationPort int    `json:"application_port"`
	// Detail explains an update (which ports changed).
	Detail string `json:"detail,omitempty"`
}

// Skip is a candidate that did not become (or stay) a project, with why.
type Skip struct {
	Name      string `json:"name"`
	Directory string `json:"directory"`
	Reason    string `json:"reason"`
}

// EntryResult is the outcome of syncing one discovery entry.
type EntryResult struct {
	Name string `json:"name"`
	// Mode is the entry's discovery mode (config.ModeProxy or
	// config.ModeWildcard).
	Mode      string `json:"mode"`
	Directory string `json:"directory,omitempty"`
	// File is the managed projects.d file (proxy mode).
	File string `json:"file,omitempty"`
	// BaseDomain, SupervisorPort, and URLPattern describe a wildcard entry:
	// its worktrees answer at URLPattern (https://<label>.<base_domain>)
	// through the one listener on SupervisorPort.
	BaseDomain     string `json:"base_domain,omitempty"`
	SupervisorPort int    `json:"supervisor_port,omitempty"`
	URLPattern     string `json:"url_pattern,omitempty"`
	// Retired marks a wildcard registration whose entry is no longer in the
	// config: sync unproxied it (see Herd) and forgot it.
	Retired bool `json:"retired,omitempty"`
	// Notices are things the user should know that are not errors (a stale
	// managed file left over from proxy mode, say).
	Notices []string `json:"notices,omitempty"`
	// Changed reports whether the file's content differs from what sync
	// computed; Written whether sync actually rewrote it (false on a dry
	// run).
	Changed   bool             `json:"changed"`
	Written   bool             `json:"written"`
	Added     []ProjectSummary `json:"added"`
	Updated   []ProjectSummary `json:"updated"`
	Unchanged []ProjectSummary `json:"unchanged"`
	Removed   []ProjectSummary `json:"removed"`
	Skipped   []Skip           `json:"skipped"`
	Herd      []HerdAction     `json:"herd"`
	// Errors are problems that stopped part or all of this entry's sync.
	Errors []string `json:"errors,omitempty"`
}

// Result is the outcome of one Sync.
type Result struct {
	ConfigPath    string         `json:"config_path"`
	DryRun        bool           `json:"dry_run"`
	HerdAvailable bool           `json:"herd_available"`
	HerdNote      string         `json:"herd_note,omitempty"`
	Entries       []*EntryResult `json:"discovery"`
}

// HasErrors reports whether any entry hit an error.
func (r *Result) HasErrors() bool {
	for _, e := range r.Entries {
		if len(e.Errors) > 0 {
			return true
		}
	}
	return false
}

// Changed reports whether any managed file changed (or would change).
func (r *Result) Changed() bool {
	for _, e := range r.Entries {
		if e.Changed {
			return true
		}
	}
	return false
}

// ManualCommands lists the Herd commands the user has to run by hand.
func (r *Result) ManualCommands() []string {
	var cmds []string
	for _, e := range r.Entries {
		for _, a := range e.Herd {
			if a.Status == HerdManual {
				cmds = append(cmds, a.Command)
			}
		}
	}
	return cmds
}

// managedMarker opens every managed file; a projects.d file that exists
// without it is never overwritten.
const managedMarker = "# Managed by herd-wake sync"

// ManagedHeader renders the comment block that opens a managed file.
func ManagedHeader(d *config.Discovery, configPath string) string {
	return fmt.Sprintf("%s — do not edit.\n# Discovery %q (%s): every `herd-wake sync` rewrites this file from the\n# discovery template in %s, so hand edits are lost.\n",
		managedMarker, d.Name, d.Directory, configPath)
}

// IsManagedFile reports whether the file at path carries the managed
// marker. A missing or empty file counts as managed (nothing to clobber).
func IsManagedFile(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte(managedMarker)), nil
}

// Sync reconciles every discovery entry of cfg with the filesystem and
// Herd. cfg must have been loaded from a file (its Path is where the
// projects.d directory is derived from). Problems with individual entries
// or candidates are reported in the result; the error is only for
// conditions that make syncing impossible.
func Sync(ctx context.Context, cfg *config.Config, opts Options) (*Result, error) {
	if cfg.Path == "" {
		return nil, errors.New("sync needs a configuration loaded from a file")
	}
	if opts.PortCommandTimeout <= 0 {
		opts.PortCommandTimeout = DefaultPortCommandTimeout
	}
	statePath := StatePath(cfg.Path)
	state, err := loadState(statePath)
	if err != nil {
		return nil, err
	}
	s := &syncer{
		ctx:   ctx,
		cfg:   cfg,
		opts:  opts,
		used:  map[int]string{},
		state: state,
	}
	for _, p := range cfg.Projects {
		s.used[p.SupervisorPort] = p.Name
		s.used[p.ApplicationPort] = p.Name
	}
	for _, d := range cfg.Discovery {
		if d.Wildcard() {
			s.used[d.SupervisorPort()] = config.DynamicSourcePrefix + d.Name
		}
	}
	res := &Result{
		ConfigPath:    cfg.Path,
		DryRun:        opts.DryRun,
		HerdAvailable: opts.Herd != nil,
		HerdNote:      opts.HerdNote,
		Entries:       []*EntryResult{},
	}
	previous := maps.Clone(state.Wildcards)
	for _, d := range cfg.Discovery {
		if d.Wildcard() {
			res.Entries = append(res.Entries, s.syncWildcard(d))
		} else {
			res.Entries = append(res.Entries, s.syncEntry(d))
		}
	}
	res.Entries = append(res.Entries, s.retireWildcards(previous)...)
	if !opts.DryRun {
		if err := saveState(statePath, s.state); err != nil {
			return nil, fmt.Errorf("write sync state: %w", err)
		}
	}
	return res, nil
}

// syncer carries one Sync's state.
type syncer struct {
	ctx  context.Context
	cfg  *config.Config
	opts Options
	// used maps every port claimed anywhere in the config (and allocated
	// during this sync) to the project holding it.
	used map[int]string
	// state is the wildcard registrations recorded so far; syncWildcard
	// and retireWildcards update it and Sync writes it back.
	state *syncState
}

// syncEntry reconciles one discovery entry.
func (s *syncer) syncEntry(d *config.Discovery) *EntryResult {
	res := &EntryResult{
		Name:      d.Name,
		Mode:      config.ModeProxy,
		Directory: d.Directory,
		File:      d.ManagedPath(s.cfg.Path),
		Added:     []ProjectSummary{},
		Updated:   []ProjectSummary{},
		Unchanged: []ProjectSummary{},
		Removed:   []ProjectSummary{},
		Skipped:   []Skip{},
		Herd:      []HerdAction{},
	}
	fail := func(format string, args ...any) *EntryResult {
		res.Errors = append(res.Errors, fmt.Sprintf(format, args...))
		return res
	}

	managed, err := IsManagedFile(res.File)
	if err != nil {
		return fail("read %s: %v", res.File, err)
	}
	if !managed {
		return fail("refusing to overwrite %s: it is not managed by herd-wake sync (its first line is not %q); move it aside or rename the discovery entry",
			res.File, managedMarker)
	}
	existing, err := config.DecodeFragment(res.File)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail("%v", err)
	}
	if existing == nil {
		existing = map[string]*config.Project{}
	}

	candidates, skips, err := scan(d)
	if err != nil {
		return fail("scan %s: %v", d.Directory, err)
	}
	res.Skipped = append(res.Skipped, skips...)

	final := map[string]*config.Project{}
	for _, c := range candidates {
		prev := existing[c.name]
		gen, skip := s.build(d, c, prev)
		if skip != nil {
			res.Skipped = append(res.Skipped, *skip)
			if prev != nil && gen != nil {
				final[c.name] = prev // kept unchanged despite the problem
				res.Unchanged = append(res.Unchanged, summarize(prev))
			}
			continue
		}
		final[c.name] = gen
		switch {
		case prev == nil:
			res.Added = append(res.Added, summarize(gen))
		case prev.Equivalent(gen):
			res.Unchanged = append(res.Unchanged, summarize(gen))
		default:
			sum := summarize(gen)
			sum.Detail = describeChange(prev, gen)
			res.Updated = append(res.Updated, sum)
		}
	}
	for _, name := range sortedNames(existing) {
		if _, ok := final[name]; !ok {
			res.Removed = append(res.Removed, summarize(existing[name]))
		}
	}
	sortSummaries(res.Unchanged)

	content, err := render(ManagedHeader(d, s.cfg.Path), final)
	if err != nil {
		return fail("render %s: %v", res.File, err)
	}
	current, err := os.ReadFile(res.File)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail("read %s: %v", res.File, err)
	}
	res.Changed = !bytes.Equal(current, content)
	if res.Changed && !s.opts.DryRun {
		if err := writeAtomic(res.File, content); err != nil {
			return fail("write %s: %v", res.File, err)
		}
		res.Written = true
	}

	s.herdActions(d, res, final, existing)
	return res
}

// build generates the project for one candidate. prev is its previous
// registration from the managed file, if any. A non-nil skip means the
// candidate is not registered this time; when gen is also non-nil, the
// previous registration should be kept unchanged (a transient failure).
func (s *syncer) build(d *config.Discovery, c candidate, prev *config.Project) (gen *config.Project, skip *Skip) {
	var allocated []int
	skipWith := func(keep bool, format string, args ...any) (*config.Project, *Skip) {
		for _, port := range allocated {
			s.release(port, c.name)
		}
		sk := &Skip{Name: c.name, Directory: c.dir, Reason: fmt.Sprintf(format, args...)}
		if keep && prev != nil {
			sk.Reason += " (previous registration kept)"
			return prev, sk
		}
		return nil, sk
	}

	if other, ok := s.cfg.Projects[c.name]; ok && other.Source != d.Source() {
		return skipWith(false, "a project named %q is already defined in %s", c.name, other.Source)
	}

	p := d.Template // copy: the template's own fields are shared verbatim
	p.Name = c.name
	p.PublicURL = d.PublicURL(c.name)
	p.WorkingDirectory = c.dir
	if len(p.Env) > 0 {
		env := make(map[string]string, len(p.Env))
		for k, v := range p.Env {
			env[k] = v
		}
		p.Env = env
	} else {
		p.Env = nil
	}

	// Never claim a Herd site that already belongs to something else.
	if prev == nil && s.opts.Herd != nil {
		if reason, err := s.herdConflict(p.PublicURL); err != nil {
			return skipWith(false, "%v", err)
		} else if reason != "" {
			return skipWith(false, "refused: %s", reason)
		}
	}

	if prev != nil {
		p.SupervisorPort = prev.SupervisorPort
	} else {
		port, ok := s.allocate(d.SupervisorPortRange, c.name)
		if !ok {
			return skipWith(false, "supervisor_port_range %v is exhausted", d.SupervisorPortRange)
		}
		allocated = append(allocated, port)
		p.SupervisorPort = port
	}

	switch {
	case d.PortCommand != "":
		port, err := RunPortCommand(s.ctx, c.dir, d.PortCommand, &d.Template, s.opts.PortCommandTimeout)
		if err != nil {
			return skipWith(true, "port_command failed: %v", err)
		}
		if owner := s.used[port]; owner != "" && owner != c.name {
			return skipWith(true, "port_command printed port %d, which project %q already uses", port, owner)
		}
		if port == p.SupervisorPort {
			return skipWith(true, "port_command printed port %d, which is this project's supervisor_port", port)
		}
		s.used[port] = c.name
		p.ApplicationPort = port
	case prev != nil:
		p.ApplicationPort = prev.ApplicationPort
	default:
		port, ok := s.allocate(d.ApplicationPortRange, c.name)
		if !ok {
			return skipWith(false, "application_port_range %v is exhausted", d.ApplicationPortRange)
		}
		allocated = append(allocated, port)
		p.ApplicationPort = port
	}
	return &p, nil
}

// herdConflict reports why the Herd site behind publicURL must not be
// claimed: a parked directory or linked site of that name, or an existing
// non-proxy site file (a secured PHP site). The reason is empty when the
// name is free, and a URL that is not a Herd site never conflicts.
func (s *syncer) herdConflict(publicURL string) (string, error) {
	host, site, ok := herd.SiteFromURL(publicURL)
	if !ok {
		return "", nil
	}
	reason, err := s.opts.Herd.Conflict(s.ctx, site)
	if err != nil {
		return "", fmt.Errorf("could not check Herd for a conflicting site: %w", err)
	}
	if reason != "" {
		return reason, nil
	}
	port, exists, err := s.opts.Herd.ProxyTarget(host)
	if err != nil {
		return "", fmt.Errorf("could not inspect Herd's site file for %s: %w", host, err)
	}
	if exists && port == 0 {
		return fmt.Sprintf("Herd already has a site file for %s that is not a proxy (%s); it looks like a secured PHP site", host, s.opts.Herd.SiteFile(host)), nil
	}
	return "", nil
}

// release gives back a port allocated to name during this sync.
func (s *syncer) release(port int, name string) {
	if s.used[port] == name {
		delete(s.used, port)
	}
}

// allocate claims the lowest port in [low, high] no project uses.
func (s *syncer) allocate(rng []int, name string) (int, bool) {
	if len(rng) != 2 {
		return 0, false
	}
	for port := rng[0]; port <= rng[1]; port++ {
		if s.used[port] == "" {
			s.used[port] = name
			return port, true
		}
	}
	return 0, false
}

// herdActions creates the missing proxies for the entry's projects and
// removes the proxies of removed projects, recording each action.
func (s *syncer) herdActions(d *config.Discovery, res *EntryResult, final, existing map[string]*config.Project) {
	record := func(a HerdAction) { res.Herd = append(res.Herd, a) }

	for _, name := range sortedNames(final) {
		p := final[name]
		host, site, ok := herd.SiteFromURL(p.PublicURL)
		if !ok {
			continue // not a Herd-style URL; nothing to proxy
		}
		a := HerdAction{Action: "proxy", Site: site, Project: name, Port: p.SupervisorPort, Command: herd.ProxyCommand(site, p.SupervisorPort)}
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
			case exists && port == p.SupervisorPort:
				a.Status, a.Detail = HerdSkipped, fmt.Sprintf("already proxied to 127.0.0.1:%d", port)
			case exists && port == 0:
				a.Status, a.Detail = HerdManual, fmt.Sprintf("Herd's site file %s is not a proxy; left alone", s.opts.Herd.SiteFile(host))
			default:
				if exists {
					a.Detail = fmt.Sprintf("re-pointed from 127.0.0.1:%d", port)
				}
				s.runHerd(&a, func() error { return s.opts.Herd.Proxy(s.ctx, site, p.SupervisorPort) })
			}
		}
		record(a)
	}

	for _, name := range sortedNames(existing) {
		if _, kept := final[name]; kept {
			continue
		}
		record(s.unproxyAction(d.HerdEnabled(), existing[name]))
	}
}

// unproxyAction removes the Herd proxy of a project being removed, when
// Herd's site file for it is the proxy sync created (proxy_pass to the
// project's supervisor port).
func (s *syncer) unproxyAction(enabled bool, p *config.Project) HerdAction {
	host, site, ok := herd.SiteFromURL(p.PublicURL)
	a := HerdAction{Action: "unproxy", Site: site, Project: p.Name, Port: p.SupervisorPort, Command: herd.UnproxyCommand(site)}
	switch {
	case !ok:
		a.Status, a.Detail = HerdSkipped, "public_url is not a Herd site"
	case !enabled:
		a.Status, a.Detail = HerdManual, "herd: false for this discovery entry"
	case s.opts.Herd == nil:
		a.Status, a.Detail = HerdManual, s.opts.HerdNote
	default:
		port, exists, err := s.opts.Herd.ProxyTarget(host)
		switch {
		case err != nil:
			a.Status, a.Detail = HerdManual, err.Error()
		case !exists:
			a.Status, a.Detail = HerdSkipped, "Herd has no site file for "+host
		case port != p.SupervisorPort:
			a.Status, a.Detail = HerdSkipped, fmt.Sprintf("Herd's site file %s does not proxy to 127.0.0.1:%d; left alone", s.opts.Herd.SiteFile(host), p.SupervisorPort)
		default:
			s.runHerd(&a, func() error { return s.opts.Herd.Unproxy(s.ctx, site) })
		}
	}
	return a
}

// runHerd runs one Herd mutation (or records it as dry-run) and sets the
// action's status.
func (s *syncer) runHerd(a *HerdAction, run func() error) {
	if s.opts.DryRun {
		a.Status = HerdDryRun
		return
	}
	if err := run(); err != nil {
		a.Status = HerdManual
		a.Detail = strings.TrimSpace(strings.Join([]string{a.Detail, err.Error()}, " "))
		return
	}
	a.Status = HerdDone
}

// summarize builds a ProjectSummary for a generated project.
func summarize(p *config.Project) ProjectSummary {
	return ProjectSummary{
		Name:            p.Name,
		Directory:       p.WorkingDirectory,
		PublicURL:       p.PublicURL,
		SupervisorPort:  p.SupervisorPort,
		ApplicationPort: p.ApplicationPort,
	}
}

// describeChange explains what differs between two registrations.
func describeChange(prev, next *config.Project) string {
	var parts []string
	if prev.ApplicationPort != next.ApplicationPort {
		parts = append(parts, fmt.Sprintf("application_port %d → %d", prev.ApplicationPort, next.ApplicationPort))
	}
	if prev.SupervisorPort != next.SupervisorPort {
		parts = append(parts, fmt.Sprintf("supervisor_port %d → %d", prev.SupervisorPort, next.SupervisorPort))
	}
	if prev.PublicURL != next.PublicURL {
		parts = append(parts, fmt.Sprintf("public_url %s → %s", prev.PublicURL, next.PublicURL))
	}
	if len(parts) == 0 {
		parts = append(parts, "template settings changed")
	}
	return strings.Join(parts, ", ")
}

func sortedNames(projects map[string]*config.Project) []string {
	names := make([]string, 0, len(projects))
	for name := range projects {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortSummaries(list []ProjectSummary) {
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
}

// fragment is the YAML shape of a projects.d file.
type fragment struct {
	Projects map[string]*config.Project `yaml:"projects"`
}

// render serialises a projects map as a projects.d file, header first.
// Map keys come out sorted, so the output is stable.
func render(header string, projects map[string]*config.Project) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header)
	if projects == nil {
		projects = map[string]*config.Project{}
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(fragment{Projects: projects}); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeAtomic writes data to path via a temporary file in the same
// directory and a rename, creating the directory if needed.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil { //nolint:gosec // a config file, meant to be readable
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
