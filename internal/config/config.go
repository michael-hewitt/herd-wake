// Package config loads and validates the herd-wake project configuration.
//
// The config is a user-level YAML file (by default
// ~/Library/Application Support/herd-wake/config.yaml) that maps project
// names to per-project settings: the public Herd URL, the loopback ports the
// supervisor and the application listen on, the command to run, readiness
// detection, and lifecycle timeouts.
//
// Next to the main file, an optional projects.d directory holds additional
// project files (see Load): tooling that registers projects automatically
// writes one file per project there, so the hand-written main file is never
// rewritten.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults applied to optional project fields that are left unset.
const (
	DefaultListenHost             = "127.0.0.1"
	DefaultStartupTimeoutSeconds  = 60
	DefaultIdleTimeoutMinutes     = 15
	DefaultReadinessStrategy      = ReadinessHTTP
	DefaultShutdownSignal         = "SIGTERM"
	DefaultShutdownTimeoutSeconds = 10
	DefaultLogRetentionDays       = 7
	// DefaultHoldMaxRequests bounds how many requests may be held at once
	// while a project cold-starts.
	DefaultHoldMaxRequests = 100
	// DefaultHoldWaitBufferSeconds is added to startup_timeout_seconds to
	// derive the default hold_max_wait_seconds: a held request should outlive
	// the startup attempt it is waiting on by a small margin, so the caller
	// sees the startup's real outcome instead of a generic wait timeout.
	DefaultHoldWaitBufferSeconds = 5
)

// Readiness strategies.
const (
	// ReadinessHTTP polls readiness_url until it answers an HTTP request.
	ReadinessHTTP = "http"
	// ReadinessTCP dials the application port until the connection succeeds.
	ReadinessTCP = "tcp"
)

// validShutdownSignals are the graceful-termination signals a project may
// configure.
var validShutdownSignals = map[string]bool{
	"SIGTERM": true,
	"SIGINT":  true,
	"SIGQUIT": true,
	"SIGHUP":  true,
	"SIGUSR1": true,
	"SIGUSR2": true,
	"SIGKILL": true,
}

// Config is the root of the herd-wake configuration file.
type Config struct {
	Projects map[string]*Project `yaml:"projects"`
	// Discovery lists worktree-discovery templates (see Discovery). Only
	// the main file may define them; `herd-wake sync` turns each into a
	// managed projects.d file.
	Discovery []*Discovery `yaml:"discovery,omitempty"`

	// Path is the absolute path of the main config file this configuration
	// was loaded from (empty for configs built in memory). The daemon
	// re-reads it on reload. Filled in by Load, not read from YAML.
	Path string `yaml:"-"`
}

// Project is the configuration for one registered project.
type Project struct {
	// Name is the project's key in the projects map. It is filled in by
	// Load, not read from YAML.
	Name string `yaml:"-"`
	// Source is the file the project was defined in, relative to the config
	// directory: the main file's name (e.g. "config.yaml") or
	// "projects.d/<file>.yaml". Filled in by Load; informational only —
	// it never affects behaviour (see Equivalent).
	Source string `yaml:"-"`

	// Required fields.
	PublicURL        string `yaml:"public_url"`
	SupervisorPort   int    `yaml:"supervisor_port"`
	ApplicationPort  int    `yaml:"application_port"`
	WorkingDirectory string `yaml:"working_directory"`
	Command          string `yaml:"command"`

	// Optional fields with defaults. They carry omitempty so a Project
	// written back to YAML (the managed files `herd-wake sync` generates)
	// lists only what was set, never the defaults.
	ReadinessStrategy     string `yaml:"readiness_strategy,omitempty"`
	ReadinessURL          string `yaml:"readiness_url,omitempty"`
	StartupTimeoutSeconds int    `yaml:"startup_timeout_seconds,omitempty"`
	IdleTimeoutMinutes    int    `yaml:"idle_timeout_minutes,omitempty"`
	// IdleTimeoutSeconds, when set, takes precedence over
	// idle_timeout_minutes. It exists primarily so tests (and impatient
	// users) can exercise idle shutdown with sub-minute timeouts; most
	// configs should use idle_timeout_minutes.
	IdleTimeoutSeconds     int    `yaml:"idle_timeout_seconds,omitempty"`
	ListenHost             string `yaml:"listen_host,omitempty"`
	AllowNonLoopback       bool   `yaml:"allow_non_loopback,omitempty"`
	ShutdownSignal         string `yaml:"shutdown_signal,omitempty"`
	ShutdownTimeoutSeconds int    `yaml:"shutdown_timeout_seconds,omitempty"`
	WebSocketsKeepAlive    *bool  `yaml:"websockets_keep_alive,omitempty"`
	LogRetentionDays       int    `yaml:"log_retention_days,omitempty"`
	HoldMaxWaitSeconds     int    `yaml:"hold_max_wait_seconds,omitempty"`
	HoldMaxRequests        int    `yaml:"hold_max_requests,omitempty"`

	// Optional fields without defaults.
	Env      map[string]string `yaml:"env,omitempty"`
	NodePath string            `yaml:"node_path,omitempty"`
	AlwaysOn bool              `yaml:"always_on,omitempty"`
	// RewriteHost, when true, makes the proxy present the dev server with
	// Host: 127.0.0.1:<application_port> instead of the public host, for
	// servers that refuse non-localhost hosts (Vite's server.allowedHosts,
	// webpack-dev-server's allowedHosts). The public host is still carried
	// in X-Forwarded-Host.
	RewriteHost bool `yaml:"rewrite_host,omitempty"`
}

// FieldError is a validation error tied to one field of one project (or,
// with Scope set to ScopeDiscovery, of one discovery entry).
type FieldError struct {
	Project string
	Field   string
	Message string
	// Scope is what Project names: a project (the default, ScopeProject)
	// or a discovery entry (ScopeDiscovery).
	Scope string
}

// FieldError scopes.
const (
	ScopeProject   = "project"
	ScopeDiscovery = "discovery"
)

func (e *FieldError) Error() string {
	scope := e.Scope
	if scope == "" {
		scope = ScopeProject
	}
	return fmt.Sprintf("%s %q: %s: %s", scope, e.Project, e.Field, e.Message)
}

// ProjectNames returns the configured project names in sorted order.
func (c *Config) ProjectNames() []string {
	names := make([]string, 0, len(c.Projects))
	for name := range c.Projects {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ProjectsDirName is the name of the optional per-project config directory
// next to the main config file.
const ProjectsDirName = "projects.d"

// ProjectsDir returns the projects.d directory belonging to the config file
// at configPath: a "projects.d" directory next to it.
func ProjectsDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), ProjectsDirName)
}

// Load reads, parses, defaults, and validates the config file at path, then
// merges the project files in the projects.d directory next to it.
//
// Every projects.d/*.yaml file (taken in name order; other names, hidden
// files, and subdirectories are ignored) has the same schema as the main
// file — a projects: map holding one project or many — and may be empty.
// Its projects are added to the main file's; a name defined more than once
// anywhere is a validation error naming both sources. A missing projects.d
// directory is fine.
//
// A missing main file is reported with an error satisfying
// errors.Is(err, os.ErrNotExist). Validation problems are collected and
// returned joined into a single error: every FieldError names the offending
// project (or discovery entry) and field.
func Load(path string) (*Config, error) {
	return LoadWithOptions(path, LoadOptions{})
}

// LoadOptions tunes LoadWithOptions for callers that repair a configuration
// rather than run it.
type LoadOptions struct {
	// SkipWorkingDirectoryCheck accepts projects whose working_directory
	// no longer exists. `herd-wake sync` and `project:remove` use it: a
	// managed project whose worktree was deleted must still load so it can
	// be removed, whereas the daemon (which would try to run it) rejects it.
	SkipWorkingDirectoryCheck bool
}

// LoadWithOptions is Load with options.
func LoadWithOptions(path string, opts LoadOptions) (*Config, error) {
	cfg, err := decodeFile(path)
	if err != nil {
		return nil, err
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	cfg.Path = path
	if cfg.Projects == nil {
		cfg.Projects = map[string]*Project{}
	}

	mainSource := filepath.Base(path)
	for name, p := range cfg.Projects {
		if p == nil {
			p = &Project{}
			cfg.Projects[name] = p
		}
		p.Name = name
		p.Source = mainSource
		p.applyDefaults()
	}
	for _, d := range cfg.Discovery {
		d.applyDefaults()
	}

	var errs []error
	fragments, err := listProjectFiles(ProjectsDir(path))
	if err != nil {
		return nil, err
	}
	for _, file := range fragments {
		frag, err := decodeFile(file)
		if err != nil {
			return nil, err
		}
		source := ProjectsDirName + "/" + filepath.Base(file)
		if len(frag.Discovery) > 0 {
			errs = append(errs, fmt.Errorf("%s: discovery entries belong in the main config file %s, not in %s/ files", source, mainSource, ProjectsDirName))
		}
		for _, name := range frag.ProjectNames() {
			if prev, ok := cfg.Projects[name]; ok {
				errs = append(errs, &FieldError{Project: name, Field: "name", Message: fmt.Sprintf(
					"defined in both %s and %s; a project name may only be defined once across %s and %s/",
					prev.Source, source, mainSource, ProjectsDirName)})
				continue
			}
			p := frag.Projects[name]
			if p == nil {
				p = &Project{}
			}
			p.Name = name
			p.Source = source
			p.applyDefaults()
			cfg.Projects[name] = p
		}
	}

	errs = append(errs, cfg.validate(opts))
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// DecodeFragment strictly decodes one projects.d-style file without applying
// defaults or validating: the projects exactly as written, keyed by name
// (Name filled in, Source left empty). Tooling that rewrites such a file
// uses it so the values it writes back are the ones that were there. A
// missing file is reported with an error satisfying errors.Is(err,
// os.ErrNotExist).
func DecodeFragment(path string) (map[string]*Project, error) {
	frag, err := decodeFile(path)
	if err != nil {
		return nil, err
	}
	if len(frag.Discovery) > 0 {
		return nil, fmt.Errorf("%s: discovery entries belong in the main config file, not in %s/ files", path, ProjectsDirName)
	}
	projects := make(map[string]*Project, len(frag.Projects))
	for name, p := range frag.Projects {
		if p == nil {
			p = &Project{}
		}
		p.Name = name
		projects[name] = p
	}
	return projects, nil
}

// decodeFile strictly decodes one config-shaped YAML file. An empty file (or
// one holding only comments) decodes to an empty Config.
func decodeFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &cfg, nil
}

// listProjectFiles returns the *.yaml files directly inside dir, sorted by
// name. A missing directory yields no files and no error.
func listProjectFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s directory %s: %w", ProjectsDirName, dir, err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files)
	return files, nil
}

// Equivalent reports whether p and o describe the same registration: every
// field is equal except Source, which only records where the definition was
// read from. The daemon uses it to tell changed projects from unchanged ones
// on reload, so moving a project between config files is not a change.
func (p *Project) Equivalent(o *Project) bool {
	a, b := *p, *o
	a.Source, b.Source = "", ""
	return reflect.DeepEqual(a, b)
}

// applyDefaults fills unset optional fields with their documented defaults.
func (p *Project) applyDefaults() {
	p.WorkingDirectory = expandHome(p.WorkingDirectory)
	if p.ListenHost == "" {
		p.ListenHost = DefaultListenHost
	}
	if p.ReadinessStrategy == "" {
		p.ReadinessStrategy = DefaultReadinessStrategy
	}
	if p.ReadinessURL == "" && p.ReadinessStrategy == ReadinessHTTP && isValidPort(p.ApplicationPort) {
		p.ReadinessURL = fmt.Sprintf("http://127.0.0.1:%d/", p.ApplicationPort)
	}
	if p.StartupTimeoutSeconds == 0 {
		p.StartupTimeoutSeconds = DefaultStartupTimeoutSeconds
	}
	if p.IdleTimeoutMinutes == 0 {
		p.IdleTimeoutMinutes = DefaultIdleTimeoutMinutes
	}
	if p.ShutdownSignal == "" {
		p.ShutdownSignal = DefaultShutdownSignal
	}
	if p.ShutdownTimeoutSeconds == 0 {
		p.ShutdownTimeoutSeconds = DefaultShutdownTimeoutSeconds
	}
	if p.WebSocketsKeepAlive == nil {
		keepAlive := true
		p.WebSocketsKeepAlive = &keepAlive
	}
	if p.LogRetentionDays == 0 {
		p.LogRetentionDays = DefaultLogRetentionDays
	}
	if p.HoldMaxWaitSeconds == 0 {
		p.HoldMaxWaitSeconds = p.StartupTimeoutSeconds + DefaultHoldWaitBufferSeconds
	}
	if p.HoldMaxRequests == 0 {
		p.HoldMaxRequests = DefaultHoldMaxRequests
	}
}

// IdleTimeout returns the project's effective idle timeout: the
// idle_timeout_seconds override when set (a testing hook), otherwise
// idle_timeout_minutes.
func (p *Project) IdleTimeout() time.Duration {
	if p.IdleTimeoutSeconds > 0 {
		return time.Duration(p.IdleTimeoutSeconds) * time.Second
	}
	return time.Duration(p.IdleTimeoutMinutes) * time.Minute
}

// Validate checks every project and discovery entry and returns all
// problems found, joined into one error. It returns nil when the
// configuration is valid.
func (c *Config) Validate() error {
	return c.validate(LoadOptions{})
}

func (c *Config) validate(opts LoadOptions) error {
	var errs []error

	type portClaim struct {
		project string
		field   string
	}
	claimed := map[int]portClaim{}

	for _, name := range c.ProjectNames() {
		p := c.Projects[name]

		if strings.TrimSpace(name) == "" {
			errs = append(errs, &FieldError{Project: name, Field: "name", Message: "project name must not be empty"})
		}

		errs = append(errs, p.validate(opts)...)

		for _, port := range []struct {
			field string
			value int
		}{
			{"supervisor_port", p.SupervisorPort},
			{"application_port", p.ApplicationPort},
		} {
			if !isValidPort(port.value) {
				continue // already reported by p.validate
			}
			if prev, ok := claimed[port.value]; ok {
				errs = append(errs, &FieldError{Project: name, Field: port.field, Message: fmt.Sprintf(
					"port %d is already used by project %q (%s); every port must be unique across the config",
					port.value, prev.project, prev.field)})
				continue
			}
			claimed[port.value] = portClaim{project: name, field: port.field}
		}
	}

	errs = append(errs, c.validateDiscovery()...)

	return errors.Join(errs...)
}

// validate checks a single project's fields, returning one FieldError per
// problem.
func (p *Project) validate(opts LoadOptions) []error {
	var errs []error
	fail := func(field, format string, args ...any) {
		errs = append(errs, &FieldError{Project: p.Name, Field: field, Message: fmt.Sprintf(format, args...)})
	}

	if p.PublicURL == "" {
		fail("public_url", "required: the Herd URL that fronts this project, e.g. https://dashboard.test")
	} else if err := checkHTTPURL(p.PublicURL); err != nil {
		fail("public_url", "invalid URL %q: %v", p.PublicURL, err)
	}

	for _, port := range []struct {
		field string
		value int
	}{
		{"supervisor_port", p.SupervisorPort},
		{"application_port", p.ApplicationPort},
	} {
		switch {
		case port.value == 0:
			fail(port.field, "required: a loopback port between 1 and 65535")
		case !isValidPort(port.value):
			fail(port.field, "invalid port %d: must be between 1 and 65535", port.value)
		}
	}
	if isValidPort(p.SupervisorPort) && p.SupervisorPort == p.ApplicationPort {
		fail("application_port", "must differ from supervisor_port (both are %d)", p.ApplicationPort)
	}

	if p.WorkingDirectory == "" {
		fail("working_directory", "required: the absolute path the command runs in")
	} else if info, err := os.Stat(p.WorkingDirectory); err != nil {
		if !opts.SkipWorkingDirectoryCheck {
			fail("working_directory", "directory %q does not exist", p.WorkingDirectory)
		}
	} else if !info.IsDir() {
		fail("working_directory", "%q is not a directory", p.WorkingDirectory)
	}

	if strings.TrimSpace(p.Command) == "" {
		fail("command", "required: the dev-server command, e.g. npm run dev -- --host 127.0.0.1 --port %d --strictPort", p.ApplicationPort)
	}

	switch p.ReadinessStrategy {
	case ReadinessHTTP:
		if p.ReadinessURL == "" {
			fail("readiness_url", "required when readiness_strategy is %q", ReadinessHTTP)
		}
	case ReadinessTCP:
		// Uses application_port; nothing extra to check.
	default:
		fail("readiness_strategy", "invalid strategy %q: must be %q or %q", p.ReadinessStrategy, ReadinessHTTP, ReadinessTCP)
	}
	if p.ReadinessURL != "" {
		if err := checkHTTPURL(p.ReadinessURL); err != nil {
			fail("readiness_url", "invalid URL %q: %v", p.ReadinessURL, err)
		}
	}

	if !isLoopbackHost(p.ListenHost) && !p.AllowNonLoopback {
		fail("listen_host", "%q is not a loopback address; herd-wake only listens on loopback unless allow_non_loopback is set to true", p.ListenHost)
	}

	for _, timeout := range []struct {
		field string
		value int
	}{
		{"startup_timeout_seconds", p.StartupTimeoutSeconds},
		{"idle_timeout_minutes", p.IdleTimeoutMinutes},
		{"idle_timeout_seconds", p.IdleTimeoutSeconds},
		{"shutdown_timeout_seconds", p.ShutdownTimeoutSeconds},
		{"log_retention_days", p.LogRetentionDays},
		{"hold_max_wait_seconds", p.HoldMaxWaitSeconds},
		{"hold_max_requests", p.HoldMaxRequests},
	} {
		if timeout.value < 0 {
			fail(timeout.field, "must not be negative (got %d)", timeout.value)
		}
	}

	if !validShutdownSignals[p.ShutdownSignal] {
		fail("shutdown_signal", "unknown signal %q: must be one of %s", p.ShutdownSignal, signalList())
	}

	return errs
}

// checkHTTPURL reports whether raw is an absolute http or https URL with a
// host.
func checkHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}

func isValidPort(port int) bool {
	return port >= 1 && port <= 65535
}

// isLoopbackHost reports whether host is "localhost" or a loopback IP
// address.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func signalList() string {
	names := make([]string, 0, len(validShutdownSignals))
	for name := range validShutdownSignals {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
