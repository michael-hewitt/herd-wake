package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/process"
	"github.com/michael-hewitt/herd-wake/internal/version"
)

// Project lifecycle states, re-exported from the process package for
// convenience in daemon-level code and tests.
const (
	StateStopped  = process.StateStopped
	StateStarting = process.StateStarting
	StateRunning  = process.StateRunning
	StateStopping = process.StateStopping
	StateFailed   = process.StateFailed
)

// projectState is the daemon's runtime record for one registered project:
// its configuration plus the supervisor owning its process lifecycle.
type projectState struct {
	project *config.Project
	proc    *process.Supervisor
}

// newProjectStates builds the daemon's project table from the config, in
// sorted name order.
func newProjectStates(cfg *config.Config, logDir string, logger *log.Logger) []*projectState {
	states := make([]*projectState, 0, len(cfg.Projects))
	for _, name := range cfg.ProjectNames() {
		p := cfg.Projects[name]
		states = append(states, &projectState{
			project: p,
			proc:    process.NewSupervisor(p, logDir, logger),
		})
	}
	return states
}

// findProject looks up a registered project by name.
func (d *Daemon) findProject(name string) (*projectState, error) {
	for _, st := range d.states {
		if st.project.Name == name {
			return st, nil
		}
	}
	return nil, fmt.Errorf("%w %q (run `herd-wake projects` to list registered projects)",
		control.ErrUnknownProject, name)
}

// projectStatus renders one project's control-API status from its supervisor
// snapshot.
func (d *Daemon) projectStatus(st *projectState) control.ProjectStatus {
	snap := st.proc.Snapshot()
	status := control.ProjectStatus{
		Name:            st.project.Name,
		PublicURL:       st.project.PublicURL,
		SupervisorPort:  st.project.SupervisorPort,
		ApplicationPort: st.project.ApplicationPort,
		State:           snap.State,
		PID:             snap.PID,
		LastExit:        snap.LastExit,
		LastError:       snap.LastError,
	}
	if !snap.StartedAt.IsZero() {
		status.UptimeSeconds = time.Since(snap.StartedAt).Seconds()
	}
	return status
}

// Status implements control.Provider.
func (d *Daemon) Status() control.StatusResponse {
	resp := control.StatusResponse{
		Version:       version.String(),
		PID:           os.Getpid(),
		StartedAt:     d.startedAt,
		UptimeSeconds: time.Since(d.startedAt).Seconds(),
		Projects:      make([]control.ProjectStatus, 0, len(d.states)),
	}
	for _, st := range d.states {
		resp.Projects = append(resp.Projects, d.projectStatus(st))
	}
	return resp
}

// StartProject implements control.Provider: it starts the named project and
// returns once it is running or its startup failed.
func (d *Daemon) StartProject(ctx context.Context, name string) (control.ProjectStatus, error) {
	st, err := d.findProject(name)
	if err != nil {
		return control.ProjectStatus{}, err
	}
	select {
	case err := <-st.proc.EnsureStarted():
		if err != nil {
			return control.ProjectStatus{}, err
		}
		return d.projectStatus(st), nil
	case <-ctx.Done():
		return control.ProjectStatus{}, ctx.Err()
	}
}

// StopProject implements control.Provider: it gracefully stops the named
// project's process group (force-killing only after its shutdown timeout).
func (d *Daemon) StopProject(ctx context.Context, name string) (control.ProjectStatus, error) {
	st, err := d.findProject(name)
	if err != nil {
		return control.ProjectStatus{}, err
	}
	if err := st.proc.Stop(ctx); err != nil {
		return control.ProjectStatus{}, err
	}
	return d.projectStatus(st), nil
}

// RestartProject implements control.Provider: stop (if needed), then start.
func (d *Daemon) RestartProject(ctx context.Context, name string) (control.ProjectStatus, error) {
	st, err := d.findProject(name)
	if err != nil {
		return control.ProjectStatus{}, err
	}
	if err := st.proc.Restart(ctx); err != nil {
		return control.ProjectStatus{}, err
	}
	return d.projectStatus(st), nil
}

// ProjectLogs implements control.Provider: recent captured output for one
// project.
func (d *Daemon) ProjectLogs(name string, maxLines int) (control.LogsResponse, error) {
	st, err := d.findProject(name)
	if err != nil {
		return control.LogsResponse{}, err
	}
	return control.LogsResponse{
		Name:    name,
		LogFile: st.proc.LogPath(),
		Lines:   st.proc.Logs(maxLines),
	}, nil
}

// stopAllProjects gracefully stops every supervised process group, in
// parallel, each bounded by its own shutdown timeout (plus margin for the
// force-kill). It runs on every daemon exit — including the panic path via
// defer — and is idempotent: supervisors that never started anything are
// no-ops, and only tracked PGIDs are ever signaled.
func (d *Daemon) stopAllProjects() {
	maxWait := 15 * time.Second
	for _, st := range d.states {
		if wait := time.Duration(st.project.ShutdownTimeoutSeconds)*time.Second + 5*time.Second; wait > maxWait {
			maxWait = wait
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), maxWait)
	defer cancel()

	var wg sync.WaitGroup
	for _, st := range d.states {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.proc.Stop(ctx); err != nil && !errors.Is(err, context.Canceled) {
				d.logger.Printf("project %q: stop during daemon shutdown: %v", st.project.Name, err)
			}
		}()
	}
	wg.Wait()
}
