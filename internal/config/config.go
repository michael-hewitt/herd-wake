// Package config loads and validates the herd-wake project configuration.
//
// The config is a user-level YAML file (by default
// ~/Library/Application Support/herd-wake/config.yaml) that maps project
// names to per-project settings: the public Herd URL, the loopback ports the
// supervisor and the application listen on, the command to run, readiness
// detection, and lifecycle timeouts.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
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
}

// Project is the configuration for one registered project.
type Project struct {
	// Name is the project's key in the projects map. It is filled in by
	// Load, not read from YAML.
	Name string `yaml:"-"`

	// Required fields.
	PublicURL        string `yaml:"public_url"`
	SupervisorPort   int    `yaml:"supervisor_port"`
	ApplicationPort  int    `yaml:"application_port"`
	WorkingDirectory string `yaml:"working_directory"`
	Command          string `yaml:"command"`

	// Optional fields with defaults.
	ReadinessStrategy     string `yaml:"readiness_strategy"`
	ReadinessURL          string `yaml:"readiness_url"`
	StartupTimeoutSeconds int    `yaml:"startup_timeout_seconds"`
	IdleTimeoutMinutes    int    `yaml:"idle_timeout_minutes"`
	// IdleTimeoutSeconds, when set, takes precedence over
	// idle_timeout_minutes. It exists primarily so tests (and impatient
	// users) can exercise idle shutdown with sub-minute timeouts; most
	// configs should use idle_timeout_minutes.
	IdleTimeoutSeconds     int    `yaml:"idle_timeout_seconds"`
	ListenHost             string `yaml:"listen_host"`
	AllowNonLoopback       bool   `yaml:"allow_non_loopback"`
	ShutdownSignal         string `yaml:"shutdown_signal"`
	ShutdownTimeoutSeconds int    `yaml:"shutdown_timeout_seconds"`
	WebSocketsKeepAlive    *bool  `yaml:"websockets_keep_alive"`
	LogRetentionDays       int    `yaml:"log_retention_days"`
	HoldMaxWaitSeconds     int    `yaml:"hold_max_wait_seconds"`
	HoldMaxRequests        int    `yaml:"hold_max_requests"`

	// Optional fields without defaults.
	Env      map[string]string `yaml:"env"`
	NodePath string            `yaml:"node_path"`
	AlwaysOn bool              `yaml:"always_on"`
}

// FieldError is a validation error tied to one field of one project.
type FieldError struct {
	Project string
	Field   string
	Message string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("project %q: %s: %s", e.Project, e.Field, e.Message)
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

// Load reads, parses, defaults, and validates the config file at path.
//
// A missing file is reported with an error satisfying
// errors.Is(err, os.ErrNotExist). Validation problems are collected and
// returned joined into a single error: every FieldError names the offending
// project and field.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	for name, p := range cfg.Projects {
		if p == nil {
			p = &Project{}
			cfg.Projects[name] = p
		}
		p.Name = name
		p.applyDefaults()
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
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

// Validate checks every project and returns all problems found, joined into
// one error. It returns nil when the configuration is valid.
func (c *Config) Validate() error {
	var errs []error

	type portClaim struct {
		project string
		field   string
	}
	claimed := map[int]portClaim{}

	for _, name := range c.ProjectNames() {
		p := c.Projects[name]

		if strings.TrimSpace(name) == "" {
			errs = append(errs, &FieldError{name, "name", "project name must not be empty"})
		}

		errs = append(errs, p.validate()...)

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
				errs = append(errs, &FieldError{name, port.field, fmt.Sprintf(
					"port %d is already used by project %q (%s); every port must be unique across the config",
					port.value, prev.project, prev.field)})
				continue
			}
			claimed[port.value] = portClaim{project: name, field: port.field}
		}
	}

	return errors.Join(errs...)
}

// validate checks a single project's fields, returning one FieldError per
// problem.
func (p *Project) validate() []error {
	var errs []error
	fail := func(field, format string, args ...any) {
		errs = append(errs, &FieldError{p.Name, field, fmt.Sprintf(format, args...)})
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
		fail("working_directory", "directory %q does not exist", p.WorkingDirectory)
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
