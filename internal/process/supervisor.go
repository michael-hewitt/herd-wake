// Package process supervises one dev-server process per project. A
// Supervisor owns the project's lifecycle state machine (stopped, starting,
// running, stopping, failed), spawns the configured command in its own
// process group, probes readiness (TCP connect or HTTP GET), captures
// combined stdout/stderr into a bounded in-memory ring buffer plus an
// on-disk log file, and stops the process group gracefully with a bounded
// force-kill fallback.
//
// The supervisor only ever signals process groups it created itself
// (Setpgid at spawn, tracked PGID); it never matches processes by name or
// port.
package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
)

// Project lifecycle states (PRD §8).
const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateFailed   = "failed"
)

const (
	// readinessPollInterval is how often readiness is probed while starting.
	readinessPollInterval = 50 * time.Millisecond
	// readinessProbeTimeout bounds a single TCP dial or HTTP GET probe.
	readinessProbeTimeout = time.Second
	// ringCapacity is how many recent output lines are kept in memory for
	// the logs endpoint.
	ringCapacity = 200
	// maxLineBytes caps a single captured line; longer output is split.
	maxLineBytes = 64 * 1024
	// groupPollInterval is how often a stopping process group is probed for
	// surviving members (kill(-pgid, 0)).
	groupPollInterval = 15 * time.Millisecond
	// killDrainTimeout bounds how long a stop waits for a SIGKILLed group to
	// disappear before giving up (SIGKILL cannot be ignored, so this only
	// fires for processes stuck in uninterruptible kernel sleep).
	killDrainTimeout = 10 * time.Second
	// forceKillNote is appended to LastExit when a stop had to SIGKILL
	// surviving group members (e.g. an intermediate shell died on the
	// graceful signal but a grandchild ignored it).
	forceKillNote = "process group force-killed"
)

// signalsByName maps the config's validated shutdown_signal whitelist to
// syscall signals.
var signalsByName = map[string]syscall.Signal{
	"SIGTERM": syscall.SIGTERM,
	"SIGINT":  syscall.SIGINT,
	"SIGQUIT": syscall.SIGQUIT,
	"SIGHUP":  syscall.SIGHUP,
	"SIGUSR1": syscall.SIGUSR1,
	"SIGUSR2": syscall.SIGUSR2,
	"SIGKILL": syscall.SIGKILL,
}

// Snapshot is a point-in-time view of a supervisor for status reporting.
type Snapshot struct {
	State string
	// PID is the direct child's process id (also the process-group id);
	// zero unless a process is currently alive.
	PID int
	// StartedAt is when the current process was spawned; zero unless a
	// process is currently alive.
	StartedAt time.Time
	// LastExit describes how the previous process ended, e.g.
	// "exit status 1" or "signal: killed". When a stop had to SIGKILL
	// surviving group members, "process group force-killed" is appended.
	// Empty while a process is alive or before the first exit.
	LastExit string
	// LastError explains the most recent failure while in StateFailed.
	LastError string
}

// Supervisor manages the process lifecycle of a single project. All methods
// are safe for concurrent use.
type Supervisor struct {
	project *config.Project
	logPath string
	logger  *log.Logger
	probe   *http.Client

	mu        sync.Mutex
	state     string
	cmd       *exec.Cmd
	pgid      int
	spawnedAt time.Time
	lastExit  string
	lastErr   string
	// abortErr, when set while starting, is the reason the supervisor is
	// deliberately killing the process (readiness timeout, stop request);
	// the exit handler reports it instead of a generic early-exit error.
	abortErr error
	// procDone is closed once the current direct child has been reaped
	// (cmd.Wait returned) and handleExit has run. Recreated on every spawn.
	// The direct child exiting does NOT mean the project is stopped: on
	// some platforms /bin/sh stays alive as an intermediate process, on
	// others it execs the real command — either way grandchildren can
	// outlive the direct child, so stops track the whole group (stopDone).
	procDone chan struct{}
	// stopDone is closed once a stop has fully finished: the entire process
	// group is gone (or SIGKILLed and drained) and the state is StateStopped.
	// Set by the Stop caller that owns the shutdown; joined by concurrent
	// Stop callers.
	stopDone chan struct{}
	// waiters are pending EnsureStarted callers for the current startup.
	waiters []chan error
	ring    *ringBuffer
}

// NewSupervisor builds a supervisor for one project. Process output is
// appended to <logDir>/<name>.log; nothing is spawned until EnsureStarted.
func NewSupervisor(p *config.Project, logDir string, logger *log.Logger) *Supervisor {
	return &Supervisor{
		project: p,
		logPath: filepath.Join(logDir, p.Name+".log"),
		logger:  logger,
		probe:   &http.Client{Timeout: readinessProbeTimeout},
		state:   StateStopped,
		ring:    newRingBuffer(ringCapacity),
	}
}

// Snapshot returns the current lifecycle state for status reporting.
func (s *Supervisor) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{State: s.state, LastExit: s.lastExit, LastError: s.lastErr}
	if s.cmd != nil && s.cmd.Process != nil {
		snap.PID = s.cmd.Process.Pid
		snap.StartedAt = s.spawnedAt
	}
	return snap
}

// Logs returns up to n recent output lines (all buffered lines when n <= 0).
func (s *Supervisor) Logs(n int) []string { return s.ring.Last(n) }

// LogPath returns the project's on-disk log file path.
func (s *Supervisor) LogPath() string { return s.logPath }

// EnsureStarted makes sure the project's process is running or starting and
// returns a channel that yields exactly one value: nil once the project is
// running (readiness succeeded), or an error describing why startup failed.
// Concurrent callers while a startup is in flight share that one startup —
// a project is never started twice. A stopped or failed project begins a
// fresh startup; a stopping project reports an error (retry after it has
// stopped, or use Restart).
//
// This is the hook a later slice's proxy hot path uses to hold a request
// until the upstream is ready.
func (s *Supervisor) EnsureStarted() <-chan error {
	done := make(chan error, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case StateRunning:
		done <- nil
	case StateStarting:
		s.waiters = append(s.waiters, done)
	case StateStopping:
		done <- fmt.Errorf("project %q is stopping; retry once it has stopped", s.project.Name)
	default: // stopped, failed
		if err := s.spawnLocked(); err != nil {
			s.state = StateFailed
			s.lastErr = err.Error()
			done <- err
			break
		}
		s.waiters = append(s.waiters, done)
	}
	return done
}

// Stop transitions a starting or running project to stopped: it sends the
// configured graceful signal to the process group, waits up to the
// configured shutdown timeout for the ENTIRE group to be gone, and SIGKILLs
// the group only if members survive past then. The direct child exiting is
// not enough — an intermediate shell can die on the graceful signal while a
// grandchild ignores it — so the project only becomes StateStopped once
// kill(-pgid, 0) reports the group empty. The escalation runs regardless of
// ctx; ctx only bounds how long the caller waits. Stopping an already
// stopped or failed project is a no-op.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	switch s.state {
	case StateStopped, StateFailed:
		s.mu.Unlock()
		return nil
	case StateStopping:
		stopDone := s.stopDone
		s.mu.Unlock()
		select {
		case <-stopDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Starting or running: this caller owns the shutdown.
	if s.state == StateStarting {
		s.abortErr = fmt.Errorf("project %q: stopped while starting", s.project.Name)
		s.notifyWaitersLocked(s.abortErr)
	}
	s.state = StateStopping
	pgid := s.pgid
	procDone := s.procDone
	stopDone := make(chan struct{})
	s.stopDone = stopDone
	sig, sigName := s.shutdownSignal()
	timeout := time.Duration(s.project.ShutdownTimeoutSeconds) * time.Second
	s.mu.Unlock()

	s.logger.Printf("project %q: stopping (sending %s to process group %d)", s.project.Name, sigName, pgid)
	// The graceful window starts at this signal.
	grace := time.NewTimer(timeout)
	s.signalGroup(pgid, sig)

	// Drain and escalate independently of the caller's ctx so the group is
	// never left running because a caller gave up waiting.
	go s.finishStop(pgid, procDone, stopDone, grace, timeout, sigName)

	select {
	case <-stopDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finishStop waits for the whole process group to disappear within the
// graceful window (measured from the graceful signal), force-kills whatever
// survives, and only then declares the project stopped by closing stopDone.
func (s *Supervisor) finishStop(pgid int, procDone <-chan struct{}, stopDone chan<- struct{}, grace *time.Timer, timeout time.Duration, sigName string) {
	defer grace.Stop()
	forced := false
	if !s.awaitGroupExit(pgid, procDone, grace.C) {
		s.logger.Printf("project %q: process group %d still alive %s after %s; sending SIGKILL",
			s.project.Name, pgid, timeout, sigName)
		s.signalGroup(pgid, syscall.SIGKILL)
		forced = true
		kill := time.NewTimer(killDrainTimeout)
		defer kill.Stop()
		if !s.awaitGroupExit(pgid, procDone, kill.C) {
			s.logger.Printf("project %q: process group %d still not gone %s after SIGKILL; declaring it stopped anyway",
				s.project.Name, pgid, killDrainTimeout)
		}
	}

	s.mu.Lock()
	s.state = StateStopped
	s.lastErr = ""
	if forced {
		// Make the exit summary truthful: the direct child's wait status
		// alone (e.g. "signal: terminated" from an intermediate shell) would
		// hide that surviving group members had to be SIGKILLed.
		if s.lastExit == "" {
			s.lastExit = forceKillNote
		} else {
			s.lastExit += "; " + forceKillNote
		}
	}
	s.mu.Unlock()
	close(stopDone)
	s.logger.Printf("project %q: process group %d fully stopped", s.project.Name, pgid)
}

// awaitGroupExit waits until the direct child has been reaped AND the
// process group has no members left, or deadline fires (returning false).
// Requiring procDone first guarantees handleExit has recorded the exit
// before the group probe can succeed.
func (s *Supervisor) awaitGroupExit(pgid int, procDone <-chan struct{}, deadline <-chan time.Time) bool {
	select {
	case <-procDone:
	case <-deadline:
		return false
	}
	ticker := time.NewTicker(groupPollInterval)
	defer ticker.Stop()
	for {
		if groupGone(pgid) {
			return true
		}
		select {
		case <-deadline:
			return false
		case <-ticker.C:
		}
	}
}

// groupGone reports whether the process group has no members left.
// kill(-pgid, 0) fails with ESRCH once every member — including zombies not
// yet reaped — is gone; EPERM can only mean the pgid was already recycled by
// another user's process, so any error means our group is gone.
func groupGone(pgid int) bool {
	return syscall.Kill(-pgid, 0) != nil
}

// Restart stops the project (if needed) and starts it again, returning once
// the new process is ready or has failed.
func (s *Supervisor) Restart(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}
	select {
	case err := <-s.EnsureStarted():
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// spawnLocked starts the project's command and the goroutines that monitor
// it. Called with s.mu held, from StateStopped or StateFailed. On success
// the state is StateStarting.
//
// The command string is executed via `/bin/sh -c` from the project's
// working directory, so it behaves exactly like the line the user would
// type in a terminal (quoting, $VARS, && chains). The shell and everything
// it spawns share one new process group, which is what stop/kill signals
// target.
func (s *Supervisor) spawnLocked() error {
	logFile, err := openLogFile(s.logPath, s.project.LogRetentionDays)
	if err != nil {
		return fmt.Errorf("project %q: %w", s.project.Name, err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		logFile.Close() //nolint:errcheck // best-effort cleanup on the error path
		return fmt.Errorf("project %q: create output pipe: %w", s.project.Name, err)
	}

	cmd := exec.Command("/bin/sh", "-c", s.project.Command)
	cmd.Dir = s.project.WorkingDirectory
	cmd.Env = s.commandEnv()
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		w.Close()       //nolint:errcheck // best-effort cleanup on the error path
		r.Close()       //nolint:errcheck // best-effort cleanup on the error path
		logFile.Close() //nolint:errcheck // best-effort cleanup on the error path
		return fmt.Errorf("project %q: start command: %w", s.project.Name, err)
	}
	// The child processes hold the write end now; closing ours makes the
	// pump see EOF once every process in the group has exited.
	w.Close() //nolint:errcheck // parent's copy; the child keeps its own descriptor

	s.state = StateStarting
	s.cmd = cmd
	s.pgid = cmd.Process.Pid // Setpgid: true makes the child lead its own group
	s.spawnedAt = time.Now()
	s.lastErr = ""
	s.lastExit = ""
	s.abortErr = nil
	procDone := make(chan struct{})
	s.procDone = procDone

	go s.pump(r, logFile)
	go func() {
		waitErr := cmd.Wait()
		s.handleExit(cmd, waitErr, procDone)
	}()
	go s.probeReadiness(cmd, procDone)

	s.logger.Printf("project %q: starting `%s` (pid %d)", s.project.Name, s.project.Command, cmd.Process.Pid)
	return nil
}

// handleExit is the single place a process exit is turned into a state
// transition. It runs (exactly once per spawn) when cmd.Wait returns —
// whether the exit was graceful, forced, deliberate (stop, readiness
// timeout), or external (someone killed the process).
func (s *Supervisor) handleExit(cmd *exec.Cmd, waitErr error, procDone chan struct{}) {
	exitDesc, cleanExit := describeExit(cmd.ProcessState, waitErr)

	s.mu.Lock()
	prev := s.state
	if s.cmd == cmd { // guard against a respawn racing an extremely late exit
		pgid := s.pgid
		s.cmd = nil
		s.pgid = 0
		s.spawnedAt = time.Time{}
		s.lastExit = exitDesc
		switch prev {
		case StateStarting:
			s.state = StateFailed
			reason := s.abortErr
			if reason == nil {
				reason = fmt.Errorf("project %q: process exited during startup (%s); see `herd-wake logs %s`",
					s.project.Name, exitDesc, s.project.Name)
			}
			s.abortErr = nil
			s.lastErr = reason.Error()
			s.notifyWaitersLocked(reason)
			// The direct child is gone but grandchildren may survive (e.g. a
			// shell that spawned a background process and exited). Nothing
			// asked them to stop gracefully, so reap the stragglers now —
			// children are never orphaned.
			s.signalGroup(pgid, syscall.SIGKILL)
		case StateStopping:
			// A Stop owns this shutdown: it declares StateStopped only once
			// the entire process group is gone (finishStop), so only the
			// direct child's exit is recorded here.
		case StateRunning:
			// Unexpected exit: we did not ask the process to stop.
			if cleanExit {
				s.state = StateStopped
				s.lastErr = ""
			} else {
				s.state = StateFailed
				s.lastErr = fmt.Sprintf("process exited unexpectedly (%s); see `herd-wake logs %s`",
					exitDesc, s.project.Name)
			}
			// Same straggler cleanup as above: whatever the direct child left
			// behind in the group must not outlive it.
			s.signalGroup(pgid, syscall.SIGKILL)
		}
	}
	next := s.state
	close(procDone)
	s.mu.Unlock()

	s.logger.Printf("project %q: process exited (%s); %s -> %s", s.project.Name, exitDesc, prev, next)
}

// probeReadiness polls the configured readiness check until it succeeds, the
// process exits, or startup_timeout_seconds elapses. On timeout it kills the
// process group and lets the exit handler transition to failed.
func (s *Supervisor) probeReadiness(cmd *exec.Cmd, procDone <-chan struct{}) {
	timeout := time.Duration(s.project.StartupTimeoutSeconds) * time.Second
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(readinessPollInterval)
	defer ticker.Stop()

	for {
		if s.probeOnce() {
			s.mu.Lock()
			if s.state == StateStarting && s.cmd == cmd {
				s.state = StateRunning
				s.notifyWaitersLocked(nil)
				s.logger.Printf("project %q: ready (pid %d)", s.project.Name, cmd.Process.Pid)
			}
			s.mu.Unlock()
			return
		}
		select {
		case <-procDone:
			return // exit already handled
		case <-deadline.C:
			s.mu.Lock()
			if s.state != StateStarting || s.cmd != cmd {
				s.mu.Unlock()
				return
			}
			s.abortErr = fmt.Errorf("project %q: not ready within %s; killed process group (see `herd-wake logs %s`)",
				s.project.Name, timeout, s.project.Name)
			pgid := s.pgid
			s.mu.Unlock()
			s.logger.Printf("project %q: readiness timed out after %s; killing process group %d",
				s.project.Name, timeout, pgid)
			s.signalGroup(pgid, syscall.SIGKILL)
			return
		case <-ticker.C:
		}
	}
}

// probeOnce runs a single readiness check. For the http strategy any HTTP
// response counts as ready (the server is answering requests, whatever the
// status code); for tcp a successful connect to the application port counts.
func (s *Supervisor) probeOnce() bool {
	if s.project.ReadinessStrategy == config.ReadinessTCP {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.project.ApplicationPort))
		conn, err := net.DialTimeout("tcp", addr, readinessProbeTimeout)
		if err != nil {
			return false
		}
		conn.Close() //nolint:errcheck // probe connection
		return true
	}
	resp, err := s.probe.Get(s.project.ReadinessURL)
	if err != nil {
		return false
	}
	resp.Body.Close() //nolint:errcheck // only reachability matters
	return true
}

// pump copies combined stdout/stderr from the process group into the ring
// buffer and the log file, line by line. It ends when every process holding
// the pipe's write end has exited (EOF). Lines longer than maxLineBytes are
// split.
func (s *Supervisor) pump(r *os.File, logFile *os.File) {
	defer r.Close()       //nolint:errcheck // read side, nothing to recover
	defer logFile.Close() //nolint:errcheck // best-effort flush on EOF
	br := bufio.NewReaderSize(r, 32*1024)
	var line []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		line = append(line, chunk...)
		if err == nil && isPrefix && len(line) < maxLineBytes {
			continue // keep accumulating this long line
		}
		if len(line) > 0 || err == nil {
			s.ring.Append(string(line))
			fmt.Fprintln(logFile, string(line))
			line = line[:0]
		}
		if err != nil {
			return
		}
	}
}

// notifyWaitersLocked delivers the startup outcome to every pending
// EnsureStarted caller. Called with s.mu held; the channels are buffered so
// abandoned callers never block delivery.
func (s *Supervisor) notifyWaitersLocked(err error) {
	for _, w := range s.waiters {
		w <- err
	}
	s.waiters = nil
}

// signalGroup signals an entire process group. pgid is only ever a group
// this supervisor created via Setpgid and still tracks; a group that has
// already died (ESRCH) is silently ignored.
func (s *Supervisor) signalGroup(pgid int, sig syscall.Signal) {
	if pgid <= 0 {
		return
	}
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		s.logger.Printf("project %q: signal %v to process group %d: %v", s.project.Name, sig, pgid, err)
	}
}

// shutdownSignal resolves the configured graceful shutdown signal.
func (s *Supervisor) shutdownSignal() (syscall.Signal, string) {
	if sig, ok := signalsByName[s.project.ShutdownSignal]; ok {
		return sig, s.project.ShutdownSignal
	}
	return syscall.SIGTERM, "SIGTERM" // config validation makes this unreachable
}

// commandEnv builds the child environment: the daemon's environment, plus a
// PATH override for node_path, plus the project's env entries. os/exec keeps
// the last occurrence of a duplicated variable, so later entries override
// earlier ones (project env wins over node_path wins over the inherited
// environment).
func (s *Supervisor) commandEnv() []string {
	env := os.Environ()
	if s.project.NodePath != "" {
		// node_path is documented as the node executable's path; prepend its
		// directory to PATH (or the path itself if it already is a directory).
		dir := s.project.NodePath
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			dir = filepath.Dir(dir)
		}
		env = append(env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	keys := make([]string, 0, len(s.project.Env))
	for k := range s.project.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+s.project.Env[k])
	}
	return env
}

// describeExit renders how a process ended ("exit status 1",
// "signal: killed", ...) and whether it was a clean zero exit.
func describeExit(ps *os.ProcessState, waitErr error) (desc string, clean bool) {
	if ps != nil {
		return ps.String(), ps.Success()
	}
	if waitErr != nil {
		return waitErr.Error(), false
	}
	return "exited", false
}
