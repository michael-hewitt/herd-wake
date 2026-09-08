// Package control implements the herd-wake control plane: a small versioned
// HTTP API served over a unix domain socket, plus the client the CLI uses to
// talk to it.
//
// The protocol is plain HTTP with JSON bodies so later slices can add
// endpoints (activity leases, ...) without a custom wire format. All paths
// are versioned under /v1/:
//
//	GET    /v1/status
//	POST   /v1/reload
//	POST   /v1/projects/{name}/start
//	POST   /v1/projects/{name}/stop
//	POST   /v1/projects/{name}/restart
//	POST   /v1/projects/{name}/lease?ttl=30m
//	DELETE /v1/projects/{name}/lease
//	GET    /v1/projects/{name}/logs?lines=N
package control

import (
	"errors"
	"time"
)

// ErrUnknownProject marks lifecycle requests naming a project that is not in
// the daemon's configuration. The server answers such requests with 404.
var ErrUnknownProject = errors.New("unknown project")

// ErrDaemonUnreachable marks client errors caused by the daemon not
// answering on its control socket (not running, stale path, ...), as opposed
// to the daemon answering with an error.
var ErrDaemonUnreachable = errors.New("daemon unreachable")

// StatusResponse is the body of GET /v1/status: daemon identity and the
// current state of every registered project.
type StatusResponse struct {
	Version       string    `json:"version"`
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	// ConfigPath is the main config file the daemon loaded and re-reads on
	// reload (empty when the daemon runs from an in-memory config).
	ConfigPath string `json:"config_path,omitempty"`
	// LastReloadAt is when the configuration was last reloaded successfully;
	// zero when it has not been reloaded since the daemon started.
	LastReloadAt time.Time       `json:"last_reload_at,omitzero"`
	Projects     []ProjectStatus `json:"projects"`
}

// Uptime returns the daemon uptime as a duration.
func (s *StatusResponse) Uptime() time.Duration {
	return time.Duration(s.UptimeSeconds * float64(time.Second))
}

// ProjectStatus is one project's entry in a StatusResponse, and the body of
// successful project lifecycle responses.
type ProjectStatus struct {
	Name            string `json:"name"`
	PublicURL       string `json:"public_url"`
	SupervisorPort  int    `json:"supervisor_port"`
	ApplicationPort int    `json:"application_port"`
	State           string `json:"state"`

	// PID and UptimeSeconds are set while the project's process is alive.
	PID           int     `json:"pid,omitempty"`
	UptimeSeconds float64 `json:"uptime_seconds,omitempty"`
	// LastExit describes how the previous process ended (e.g.
	// "exit status 1", "signal: killed"); empty while running or before the
	// first exit.
	LastExit string `json:"last_exit,omitempty"`
	// LastError explains the most recent failure while the state is failed.
	LastError string `json:"last_error,omitempty"`

	// AlwaysOn marks projects that start with the daemon and are never
	// idle-stopped.
	AlwaysOn bool `json:"always_on,omitempty"`
	// InflightRequests is how many proxied requests are in flight right
	// now; any in-flight request parks the idle countdown.
	InflightRequests int64 `json:"inflight_requests,omitempty"`
	// LastActivityAt is when the most recent proxied request completed;
	// zero when none has completed since the daemon started.
	LastActivityAt time.Time `json:"last_activity_at,omitzero"`
	// LeaseUntil is when the project's activity lease expires; zero when no
	// lease is active. An active lease parks the idle countdown.
	LeaseUntil time.Time `json:"lease_until,omitzero"`
	// IdleStopAt is when the project is scheduled to be idle-stopped. Zero
	// when it is not running, is always_on, or the countdown is parked by
	// in-flight requests or an active lease.
	IdleStopAt time.Time `json:"idle_stop_at,omitzero"`
}

// ReloadResponse is the body of POST /v1/reload: the outcome of re-reading
// the configuration and diffing it against the running project set.
type ReloadResponse struct {
	// ConfigPath is the main config file that was (re-)read.
	ConfigPath string `json:"config_path"`
	// Applied is false when the reload was rejected outright — the config
	// failed to load or validate — and the running state is untouched.
	// Errors then holds the reasons. When true, the diff below was applied;
	// Errors may still list per-project problems (e.g. a bind failure for
	// an added project) that did not abort the rest of the reload.
	Applied bool `json:"applied"`
	// ReloadedAt is when the reload was applied; zero when rejected.
	ReloadedAt time.Time `json:"reloaded_at,omitzero"`
	// Added, Removed, Changed, and Unchanged list project names (sorted) by
	// how the reload treated them.
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	Changed   []string `json:"changed"`
	Unchanged []string `json:"unchanged"`
	// Errors are validation errors (Applied false) or per-project apply
	// errors (Applied true), one message each.
	Errors []string `json:"errors,omitempty"`
}

// LogsResponse is the body of GET /v1/projects/{name}/logs: recent combined
// stdout/stderr of the project's process.
type LogsResponse struct {
	Name string `json:"name"`
	// LogFile is the daemon-side path of the full on-disk log.
	LogFile string `json:"log_file"`
	// Lines are the most recent captured output lines, oldest first.
	Lines []string `json:"lines"`
}

// errorResponse is the JSON body of every non-200 answer.
type errorResponse struct {
	Error string `json:"error"`
}
