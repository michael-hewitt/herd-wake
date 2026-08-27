// Package control implements the herd-wake control plane: a small versioned
// HTTP API served over a unix domain socket, plus the client the CLI uses to
// talk to it.
//
// The protocol is plain HTTP with JSON bodies so later slices can add
// endpoints (project start/stop/restart, logs, activity leases) without a
// custom wire format. All paths are versioned under /v1/.
package control

import "time"

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

// ProjectStatus is one project's entry in a StatusResponse.
type ProjectStatus struct {
	Name            string `json:"name"`
	PublicURL       string `json:"public_url"`
	SupervisorPort  int    `json:"supervisor_port"`
	ApplicationPort int    `json:"application_port"`
	State           string `json:"state"`
}
