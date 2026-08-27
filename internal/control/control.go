// Package control implements the herd-wake control plane: a small versioned
// HTTP API served over a unix domain socket, plus the client the CLI uses to
// talk to it.
//
// The protocol is plain HTTP with JSON bodies so later slices can add
// endpoints (activity leases, ...) without a custom wire format. All paths
// are versioned under /v1/:
//
//	GET  /v1/status
//	POST /v1/projects/{name}/start
//	POST /v1/projects/{name}/stop
//	POST /v1/projects/{name}/restart
//	GET  /v1/projects/{name}/logs?lines=N
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
	Version       string          `json:"version"`
	PID           int             `json:"pid"`
	StartedAt     time.Time       `json:"started_at"`
	UptimeSeconds float64         `json:"uptime_seconds"`
	Projects      []ProjectStatus `json:"projects"`
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
