package daemon

import "github.com/michael-hewitt/herd-wake/internal/config"

// Project lifecycle states as reported over the control socket. Slice 4
// introduces the real state machine behind these; in this slice every
// project reports StateStopped because the daemon does not manage processes
// yet (upstreams are started manually).
const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateFailed   = "failed"
)

// projectState is the daemon's runtime record for one registered project.
// Slice 4 replaces the static state string with a real state machine (and
// the supervised process handle); the shape exists now so the control API
// and proxy wiring do not have to change.
type projectState struct {
	project *config.Project
	state   string
}

// newProjectStates builds the daemon's project table from the config, in
// sorted name order.
func newProjectStates(cfg *config.Config) []*projectState {
	states := make([]*projectState, 0, len(cfg.Projects))
	for _, name := range cfg.ProjectNames() {
		states = append(states, &projectState{
			project: cfg.Projects[name],
			state:   StateStopped,
		})
	}
	return states
}
