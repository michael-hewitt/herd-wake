package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Discovery defaults.
const (
	// DefaultURLTemplate is the public URL template a discovery entry uses
	// when url_template is unset: {name} becomes the worktree's directory
	// name.
	DefaultURLTemplate = "https://{name}.test"
	// URLTemplateName is the placeholder url_template must contain.
	URLTemplateName = "{name}"
)

// DefaultExclude is the exclude list a discovery entry uses when none is
// set: dot-directories are never candidates.
var DefaultExclude = []string{".*"}

// Discovery is one worktree-discovery template from the main config file's
// discovery: list. `herd-wake sync` scans Directory, and for every git
// worktree it finds writes a project built from the template — plus the
// generated public_url, supervisor_port, application_port, and
// working_directory — into the managed file projects.d/<Name>.yaml.
type Discovery struct {
	// Name identifies the entry; the managed file is projects.d/<Name>.yaml.
	// Must be a DNS label.
	Name string `yaml:"name"`
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
	// URLTemplate builds each project's public_url; {name} is replaced by
	// the directory name. Default: DefaultURLTemplate.
	URLTemplate string `yaml:"url_template,omitempty"`
	// PortCommand, when set, is run in the worktree (via /bin/sh -c, with
	// the template's env and node_path applied) on every sync; its output —
	// a port number or a URL — supplies application_port.
	PortCommand string `yaml:"port_command,omitempty"`
	// ApplicationPortRange is [low, high]: without PortCommand, a new
	// project gets the lowest port in it not used anywhere in the config.
	ApplicationPortRange []int `yaml:"application_port_range,omitempty"`
	// SupervisorPortRange is [low, high]: a new project gets the lowest
	// port in it not used anywhere in the config. Allocated ports are
	// written to the managed file and never change afterwards.
	SupervisorPortRange []int `yaml:"supervisor_port_range"`
	// Herd controls whether sync creates and removes Herd proxy entries for
	// this entry's projects. Unset means true.
	Herd *bool `yaml:"herd,omitempty"`
	// Exclude holds glob patterns (path.Match) on the directory name; a
	// matching subdirectory is never a candidate. Default: DefaultExclude.
	Exclude []string `yaml:"exclude,omitempty"`

	// Template holds every per-project field. command is required; the
	// generated fields (public_url, supervisor_port, application_port,
	// working_directory) are rejected. Everything else is copied verbatim
	// to each generated project.
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

// FileName returns the managed file's name inside projects.d.
func (d *Discovery) FileName() string {
	return d.Name + ".yaml"
}

// Source returns the Project.Source value of projects loaded from this
// entry's managed file ("projects.d/<name>.yaml").
func (d *Discovery) Source() string {
	return ProjectsDirName + "/" + d.FileName()
}

// ManagedPath returns the absolute path of this entry's managed file next to
// the config file at configPath.
func (d *Discovery) ManagedPath(configPath string) string {
	return filepath.Join(ProjectsDir(configPath), d.FileName())
}

// PublicURL renders the url_template for a project name.
func (d *Discovery) PublicURL(name string) string {
	return strings.ReplaceAll(d.URLTemplate, URLTemplateName, name)
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
// projects when they are loaded, so managed files never pin them.
func (d *Discovery) applyDefaults() {
	d.Directory = expandHome(d.Directory)
	d.Repository = expandHome(d.Repository)
	if d.URLTemplate == "" {
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
		fail("name", "required: identifies the entry and names its managed file %s/<name>.yaml", ProjectsDirName)
	case !IsDNSLabel(d.Name):
		fail("name", "%q is not a valid DNS label (lowercase letters, digits, and hyphens; at most 63 characters)", d.Name)
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

	if !strings.Contains(d.URLTemplate, URLTemplateName) {
		fail("url_template", "%q must contain %s (replaced by the worktree's directory name)", d.URLTemplate, URLTemplateName)
	} else if err := checkHTTPURL(d.PublicURL("example")); err != nil {
		fail("url_template", "invalid URL template %q: %v", d.URLTemplate, err)
	}

	errs = append(errs, d.validateRange(label, "supervisor_port_range", d.SupervisorPortRange, true)...)
	errs = append(errs, d.validateRange(label, "application_port_range", d.ApplicationPortRange, d.PortCommand == "")...)

	for _, pattern := range d.Exclude {
		if _, err := path.Match(pattern, ""); err != nil {
			fail("exclude", "invalid pattern %q: %v", pattern, err)
		}
	}

	// The generated fields must come from the template's own settings.
	t := d.Template
	if t.PublicURL != "" {
		fail("public_url", "not allowed in a discovery template: public_url is generated from url_template")
	}
	if t.SupervisorPort != 0 {
		fail("supervisor_port", "not allowed in a discovery template: supervisor_port is allocated from supervisor_port_range")
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
	probe.PublicURL = d.PublicURL("example")
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
		case "public_url", "supervisor_port", "application_port", "working_directory", "readiness_url":
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
