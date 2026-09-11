package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
)

// Discovery modes.
const (
	// ModeProxy is the per-worktree mode: `herd-wake sync` registers one
	// project — with its own supervisor_port and Herd proxy — per worktree
	// in the managed file projects.d/<name>.yaml.
	ModeProxy = "proxy"
	// ModeWildcard is the shared-listener mode: the daemon binds one
	// supervisor_port for the whole entry and resolves <label>.<base_domain>
	// to the worktree <directory>/<label> on demand. Nothing is registered
	// per worktree; `herd-wake sync` only ensures a single wildcard Herd
	// proxy for base_domain exists.
	ModeWildcard = "wildcard"
)

// Discovery defaults.
const (
	// DefaultURLTemplate is the public URL template a proxy-mode entry uses
	// when url_template is unset: {name} becomes the worktree's directory
	// name.
	DefaultURLTemplate = "https://{name}.test"
	// URLTemplateName is the placeholder url_template must contain.
	URLTemplateName = "{name}"
	// DefaultBaseDomainSuffix is appended to a wildcard entry's name to
	// form its default base_domain (<name>.test).
	DefaultBaseDomainSuffix = ".test"
	// DynamicSourcePrefix opens the Source of a project a wildcard entry
	// materialised at runtime: "discovery:<entry name>".
	DynamicSourcePrefix = "discovery:"
	// LabelPlaceholder stands for the worktree label in a wildcard entry's
	// URL pattern (see Discovery.URLPattern).
	LabelPlaceholder = "<label>"
)

// DefaultExclude is the exclude list a discovery entry uses when none is
// set: dot-directories are never candidates.
var DefaultExclude = []string{".*"}

// Discovery is one worktree-discovery template from the main config file's
// discovery: list. Its Directory holds git worktrees, one per subdirectory;
// each becomes a project built from the template. How and when depends on
// Mode:
//
//   - proxy (the default): `herd-wake sync` scans Directory and writes one
//     generated project per worktree — plus a generated public_url,
//     supervisor_port, application_port, and working_directory — into the
//     managed file projects.d/<Name>.yaml, and registers a Herd proxy per
//     worktree.
//   - wildcard: the daemon binds the entry's supervisor_port once and
//     resolves the request's Host header, <label>.<base_domain>, to the
//     worktree Directory/<label> when it is first seen; the project exists
//     in memory only. One Herd proxy for base_domain covers every label.
type Discovery struct {
	// Name identifies the entry. Must be a DNS label: in proxy mode it names
	// the managed file projects.d/<Name>.yaml, in wildcard mode it is the
	// default base_domain's first label.
	Name string `yaml:"name"`
	// Mode is ModeProxy (default) or ModeWildcard.
	Mode string `yaml:"mode,omitempty"`
	// BaseDomain (wildcard mode) is the domain under which worktree labels
	// are served: the worktree <Directory>/<label> answers at
	// https://<label>.<BaseDomain>. Default: <Name>.test. Its Herd site is
	// BaseDomain without its TLD.
	BaseDomain string `yaml:"base_domain,omitempty"`
	// Directory is scanned: its immediate subdirectories are candidates.
	// A leading ~/ is expanded.
	Directory string `yaml:"directory"`
	// Repository, when set, restricts candidates to linked worktrees of
	// this git repository (the candidate's .git file points into
	// <Repository>/.git/worktrees/). Standalone repositories and worktrees
	// of other repositories in Directory are ignored. A leading ~/ is
	// expanded.
	Repository string `yaml:"repository,omitempty"`
	// RequireFiles lists paths (relative to the candidate) that must all
	// exist for it to become a project.
	RequireFiles []string `yaml:"require_files,omitempty"`
	// URLTemplate (proxy mode) builds each project's public_url; {name} is
	// replaced by the directory name. Default: DefaultURLTemplate. Not used
	// in wildcard mode, where the URL is https://<label>.<base_domain>.
	URLTemplate string `yaml:"url_template,omitempty"`
	// PortCommand, when set, is run in the worktree (via /bin/sh -c, with
	// the template's env and node_path applied); its output — a port number
	// or a URL — supplies application_port. Proxy mode runs it on every
	// sync; wildcard mode runs it when a label is materialised (its first
	// request, or the first after its worktree was re-created) and on
	// project:restart, and the project keeps the port in between.
	PortCommand string `yaml:"port_command,omitempty"`
	// ApplicationPortRange is [low, high]: without PortCommand, a new
	// project gets the lowest port in it not used anywhere in the config
	// (proxy mode) or by any live project (wildcard mode).
	ApplicationPortRange []int `yaml:"application_port_range,omitempty"`
	// SupervisorPortRange (proxy mode) is [low, high]: a new project gets
	// the lowest port in it not used anywhere in the config. Allocated
	// ports are written to the managed file and never change afterwards.
	// Wildcard mode has one shared supervisor_port instead (see Template).
	SupervisorPortRange []int `yaml:"supervisor_port_range,omitempty"`
	// Herd controls whether sync creates and removes Herd proxy entries for
	// this entry. Unset means true.
	Herd *bool `yaml:"herd,omitempty"`
	// Exclude holds glob patterns (path.Match) on the directory name; a
	// matching subdirectory is never a candidate. Default: DefaultExclude.
	Exclude []string `yaml:"exclude,omitempty"`

	// Template holds every per-project field. command is required; the
	// generated fields (public_url, host, application_port,
	// working_directory) are rejected. supervisor_port is rejected in proxy
	// mode (allocated from SupervisorPortRange) and required in wildcard
	// mode, where it is the entry's one shared listener port. Everything
	// else is copied verbatim to each generated project.
	Template Project `yaml:",inline"`
}

// dnsLabel matches a lowercase DNS label: letters, digits, and inner
// hyphens, at most 63 characters.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// IsDNSLabel reports whether name is a valid lowercase DNS label — the form
// project and discovery names must take so they can become <name>.test
// hostnames and file names.
func IsDNSLabel(name string) bool {
	return dnsLabel.MatchString(name)
}

// IsHostname reports whether host is a valid lowercase hostname: one or
// more DNS labels joined by dots, at most 253 characters, with no scheme,
// port, or path.
func IsHostname(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !IsDNSLabel(label) {
			return false
		}
	}
	return true
}

// Wildcard reports whether the entry is in wildcard (shared-listener) mode.
func (d *Discovery) Wildcard() bool {
	return d.Mode == ModeWildcard
}

// SupervisorPort returns a wildcard entry's shared listener port (the
// template's supervisor_port); 0 for a proxy-mode entry.
func (d *Discovery) SupervisorPort() int {
	if !d.Wildcard() {
		return 0
	}
	return d.Template.SupervisorPort
}

// ListenHost returns the address a wildcard entry's listener binds: the
// template's listen_host, defaulting like a project's.
func (d *Discovery) ListenHost() string {
	if d.Template.ListenHost == "" {
		return DefaultListenHost
	}
	return d.Template.ListenHost
}

// FileName returns the managed file's name inside projects.d (proxy mode).
func (d *Discovery) FileName() string {
	return d.Name + ".yaml"
}

// Source returns the Project.Source value of projects loaded from this
// entry's managed file ("projects.d/<name>.yaml").
func (d *Discovery) Source() string {
	return ProjectsDirName + "/" + d.FileName()
}

// DynamicSource returns the Project.Source value of projects a wildcard
// entry materialises at runtime ("discovery:<name>").
func (d *Discovery) DynamicSource() string {
	return DynamicSourcePrefix + d.Name
}

// ManagedPath returns the absolute path of this entry's managed file next to
// the config file at configPath.
func (d *Discovery) ManagedPath(configPath string) string {
	return filepath.Join(ProjectsDir(configPath), d.FileName())
}

// PublicURL renders the public URL of the worktree named label: the
// url_template in proxy mode, https://<label>.<base_domain> in wildcard
// mode.
func (d *Discovery) PublicURL(label string) string {
	if d.Wildcard() {
		return "https://" + d.Host(label)
	}
	return strings.ReplaceAll(d.URLTemplate, URLTemplateName, label)
}

// Host returns the hostname a wildcard entry serves the worktree named
// label at: <label>.<base_domain>.
func (d *Discovery) Host(label string) string {
	return label + "." + d.BaseDomain
}

// URLPattern renders a wildcard entry's URL scheme for humans:
// https://<label>.<base_domain>.
func (d *Discovery) URLPattern() string {
	return "https://" + LabelPlaceholder + "." + d.BaseDomain
}

// Label extracts the worktree label from a request host on a wildcard
// entry's listener: host must be exactly <label>.<base_domain> (already
// lowercased and port-stripped) with label a DNS label. The base domain
// itself, deeper names such as www.<label>.<base_domain>, and hosts under
// other domains yield ok == false.
func (d *Discovery) Label(host string) (label string, ok bool) {
	label, ok = strings.CutSuffix(host, "."+d.BaseDomain)
	if !ok || !IsDNSLabel(label) {
		return "", false
	}
	return label, true
}

// Project builds the project a wildcard entry serves for the worktree
// named label at dir with the given application port: the template's
// fields plus the generated name, host, public_url, working_directory,
// ports, and Source (DynamicSource), with defaults applied like a loaded
// project. The template's env map is copied, never shared.
func (d *Discovery) Project(label, dir string, applicationPort int) *Project {
	p := d.Template // copy
	p.Name = label
	p.Source = d.DynamicSource()
	p.Host = d.Host(label)
	p.PublicURL = d.PublicURL(label)
	p.WorkingDirectory = dir
	p.SupervisorPort = d.SupervisorPort()
	p.ApplicationPort = applicationPort
	p.Env = copyEnv(p.Env)
	p.applyDefaults()
	return &p
}

// copyEnv returns a copy of env (nil for an empty map).
func copyEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

// Equivalent reports whether d and o describe the same entry: every field
// is equal. The daemon uses it on reload to tell changed wildcard entries
// (whose listener and dynamic projects are rebuilt) from unchanged ones.
func (d *Discovery) Equivalent(o *Discovery) bool {
	return reflect.DeepEqual(*d, *o)
}

// HerdEnabled reports whether sync manages Herd proxies for this entry.
func (d *Discovery) HerdEnabled() bool {
	return d.Herd == nil || *d.Herd
}

// Excluded reports whether a directory name matches one of the entry's
// exclude patterns.
func (d *Discovery) Excluded(name string) bool {
	for _, pattern := range d.Exclude {
		if ok, err := path.Match(pattern, name); err == nil && ok {
			return true
		}
	}
	return false
}

// applyDefaults fills unset optional discovery fields and expands ~ in
// paths. The template is left raw: defaults are applied to generated
// projects when they are loaded (or materialised), so managed files never
// pin them.
func (d *Discovery) applyDefaults() {
	d.Directory = expandHome(d.Directory)
	d.Repository = expandHome(d.Repository)
	if d.Mode == "" {
		d.Mode = ModeProxy
	}
	if d.Wildcard() {
		if d.BaseDomain == "" && d.Name != "" {
			d.BaseDomain = d.Name + DefaultBaseDomainSuffix
		}
		d.BaseDomain = strings.ToLower(strings.TrimSpace(d.BaseDomain))
	} else if d.URLTemplate == "" {
		d.URLTemplate = DefaultURLTemplate
	}
	if d.Exclude == nil {
		d.Exclude = append([]string(nil), DefaultExclude...)
	}
}

// validateDiscovery checks every discovery entry, returning one FieldError
// per problem.
func (c *Config) validateDiscovery() []error {
	var errs []error
	seen := map[string]int{}
	for i, d := range c.Discovery {
		if d == nil {
			errs = append(errs, &FieldError{Scope: ScopeDiscovery, Project: fmt.Sprintf("#%d", i+1), Field: "name", Message: "entry is empty"})
			continue
		}
		label := d.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		if prev, ok := seen[d.Name]; ok && d.Name != "" {
			errs = append(errs, &FieldError{Scope: ScopeDiscovery, Project: label, Field: "name", Message: fmt.Sprintf(
				"%q is already used by discovery entry #%d; names must be unique", d.Name, prev+1)})
		}
		seen[d.Name] = i
		errs = append(errs, d.validate(label)...)
	}
	return errs
}

// validate checks one discovery entry's fields.
func (d *Discovery) validate(label string) []error {
	var errs []error
	fail := func(field, format string, args ...any) {
		errs = append(errs, &FieldError{Scope: ScopeDiscovery, Project: label, Field: field, Message: fmt.Sprintf(format, args...)})
	}

	switch {
	case d.Name == "":
		fail("name", "required: identifies the entry (its managed file %s/<name>.yaml, or its default base_domain <name>.test)", ProjectsDirName)
	case !IsDNSLabel(d.Name):
		fail("name", "%q is not a valid DNS label (lowercase letters, digits, and hyphens; at most 63 characters)", d.Name)
	}

	switch d.Mode {
	case ModeProxy, ModeWildcard:
	default:
		fail("mode", "unknown mode %q: must be %q (one project and Herd proxy per worktree, via sync) or %q (one shared listener, worktrees resolved from the Host header on demand)", d.Mode, ModeProxy, ModeWildcard)
	}

	if d.Directory == "" {
		fail("directory", "required: the directory whose subdirectories are scanned for worktrees")
	} else if info, err := os.Stat(d.Directory); err != nil {
		fail("directory", "directory %q does not exist", d.Directory)
	} else if !info.IsDir() {
		fail("directory", "%q is not a directory", d.Directory)
	}

	if d.Repository != "" {
		if info, err := os.Stat(d.Repository); err != nil {
			fail("repository", "directory %q does not exist", d.Repository)
		} else if !info.IsDir() {
			fail("repository", "%q is not a directory", d.Repository)
		} else if _, err := os.Stat(filepath.Join(d.Repository, ".git")); err != nil {
			fail("repository", "%q is not a git repository (no .git inside)", d.Repository)
		}
	}

	for _, file := range d.RequireFiles {
		if strings.TrimSpace(file) == "" {
			fail("require_files", "entries must not be empty")
		} else if filepath.IsAbs(file) {
			fail("require_files", "%q must be relative to the worktree", file)
		}
	}

	t := d.Template
	if d.Wildcard() {
		switch {
		case d.BaseDomain == "":
			fail("base_domain", "required in wildcard mode: the domain worktrees are served under, e.g. webapp.test (default: <name>.test)")
		case !IsHostname(d.BaseDomain) || !strings.Contains(d.BaseDomain, "."):
			fail("base_domain", "invalid domain %q: expected at least two dot-separated DNS labels (lowercase letters, digits, hyphens), e.g. webapp.test", d.BaseDomain)
		}
		if d.URLTemplate != "" {
			fail("url_template", "not used in wildcard mode: every worktree is served at %s", d.URLPattern())
		}
		if len(d.SupervisorPortRange) > 0 {
			fail("supervisor_port_range", "not used in wildcard mode: set supervisor_port, the one port the shared listener binds")
		}
		switch {
		case t.SupervisorPort == 0:
			fail("supervisor_port", "required in wildcard mode: the loopback port of the entry's shared listener (the `herd proxy` target), e.g. 41000")
		case !isValidPort(t.SupervisorPort):
			fail("supervisor_port", "invalid port %d: must be between 1 and 65535", t.SupervisorPort)
		}
	} else {
		if d.BaseDomain != "" {
			fail("base_domain", "only used in wildcard mode (mode: wildcard); proxy mode builds each URL from url_template")
		}
		if !strings.Contains(d.URLTemplate, URLTemplateName) {
			fail("url_template", "%q must contain %s (replaced by the worktree's directory name)", d.URLTemplate, URLTemplateName)
		} else if err := checkHTTPURL(d.PublicURL("example")); err != nil {
			fail("url_template", "invalid URL template %q: %v", d.URLTemplate, err)
		}
		errs = append(errs, d.validateRange(label, "supervisor_port_range", d.SupervisorPortRange, true)...)
		if t.SupervisorPort != 0 {
			fail("supervisor_port", "not allowed in a proxy-mode discovery template: supervisor_port is allocated from supervisor_port_range (in wildcard mode it is the shared listener port)")
		}
	}
	errs = append(errs, d.validateRange(label, "application_port_range", d.ApplicationPortRange, d.PortCommand == "")...)

	for _, pattern := range d.Exclude {
		if _, err := path.Match(pattern, ""); err != nil {
			fail("exclude", "invalid pattern %q: %v", pattern, err)
		}
	}

	// The generated fields must come from the template's own settings.
	if t.PublicURL != "" {
		fail("public_url", "not allowed in a discovery template: public_url is generated per worktree")
	}
	if t.Host != "" {
		fail("host", "not allowed in a discovery template: the host is generated per worktree")
	}
	if t.ApplicationPort != 0 {
		fail("application_port", "not allowed in a discovery template: application_port comes from port_command or application_port_range")
	}
	if t.WorkingDirectory != "" {
		fail("working_directory", "not allowed in a discovery template: working_directory is the worktree itself")
	}

	// Validate the rest of the template the way a generated project will be
	// validated: give a probe copy valid generated fields and keep every
	// error that is not about those.
	probe := t
	probe.Name = label
	probe.PublicURL = "https://example.test"
	probe.Host = ""
	probe.SupervisorPort = 1
	probe.ApplicationPort = 2
	probe.WorkingDirectory = d.Directory
	probe.applyDefaults()
	for _, err := range probe.validate(LoadOptions{SkipWorkingDirectoryCheck: true}) {
		fe, ok := err.(*FieldError)
		if !ok {
			errs = append(errs, err)
			continue
		}
		switch fe.Field {
		case "public_url", "host", "supervisor_port", "application_port", "working_directory", "readiness_url":
			continue // generated, or derived from a generated field
		}
		fe.Scope = ScopeDiscovery
		errs = append(errs, fe)
	}
	return errs
}

// validateRange checks a [low, high] port range. When required is false an
// absent range is fine.
func (d *Discovery) validateRange(label, field string, r []int, required bool) []error {
	fail := func(format string, args ...any) []error {
		return []error{&FieldError{Scope: ScopeDiscovery, Project: label, Field: field, Message: fmt.Sprintf(format, args...)}}
	}
	if len(r) == 0 {
		if required {
			return fail("required: [low, high], the loopback ports to allocate from, e.g. [41000, 41999]")
		}
		return nil
	}
	if len(r) != 2 {
		return fail("must be [low, high] (got %d values)", len(r))
	}
	if !isValidPort(r[0]) || !isValidPort(r[1]) {
		return fail("ports must be between 1 and 65535 (got [%d, %d])", r[0], r[1])
	}
	if r[0] > r[1] {
		return fail("low must not exceed high (got [%d, %d])", r[0], r[1])
	}
	return nil
}
