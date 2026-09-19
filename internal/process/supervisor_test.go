package process

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// TestMain lets the test binary double as the spawned child processes.
func TestMain(m *testing.M) {
	testproc.Main() // exits the process when running as a helper
	os.Exit(m.Run())
}

// freePort reserves and releases a loopback port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}

// helperProject builds a project whose command re-invokes the test binary in
// the given testproc mode, listening on the application port with TCP
// readiness.
func helperProject(t *testing.T, mode string) *config.Project {
	t.Helper()
	command, err := testproc.Command()
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	return &config.Project{
		Name:                   "demo",
		PublicURL:              "https://demo.test",
		SupervisorPort:         freePort(t),
		ApplicationPort:        port,
		WorkingDirectory:       t.TempDir(),
		Command:                command,
		ReadinessStrategy:      config.ReadinessTCP,
		StartupTimeoutSeconds:  10,
		ShutdownSignal:         "SIGTERM",
		ShutdownTimeoutSeconds: 5,
		LogRetentionDays:       7,
		Env: map[string]string{
			testproc.EnvMode: mode,
			testproc.EnvPort: strconv.Itoa(port),
		},
	}
}

// shellProject builds a project running a plain shell command (no helper).
func shellProject(t *testing.T, command string) *config.Project {
	t.Helper()
	p := helperProject(t, "")
	p.Command = command
	p.Env = nil
	return p
}

func newTestSupervisor(t *testing.T, p *config.Project) *Supervisor {
	t.Helper()
	s := NewSupervisor(p, t.TempDir(), log.New(io.Discard, "", 0))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.Stop(ctx); err != nil {
			t.Errorf("cleanup stop: %v", err)
		}
	})
	return s
}

// awaitStartup waits for the outcome of an EnsureStarted channel.
func awaitStartup(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("EnsureStarted never reported an outcome")
		return nil
	}
}

func waitForState(t *testing.T, s *Supervisor, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.Snapshot().State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state = %q, want %q", s.Snapshot().State, want)
}

// processGone reports whether pid no longer exists.
func processGone(pid int) bool {
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists", pid)
}

// readPidfile polls until the helper has written its pid.
func readPidfile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("bad pidfile %q: %v", data, err)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pidfile %s never appeared", path)
	return 0
}

func TestStartTCPReadinessAndStop(t *testing.T) {
	s := newTestSupervisor(t, helperProject(t, testproc.ModeListen))
	if got := s.Snapshot().State; got != StateStopped {
		t.Fatalf("initial state = %q, want %q", got, StateStopped)
	}

	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}

	snap := s.Snapshot()
	if snap.State != StateRunning {
		t.Errorf("state = %q, want %q", snap.State, StateRunning)
	}
	if snap.PID <= 0 {
		t.Errorf("PID = %d, want a live pid", snap.PID)
	}
	if snap.StartedAt.IsZero() {
		t.Error("StartedAt should be set while running")
	}

	lines := strings.Join(s.Logs(0), "\n")
	if !strings.Contains(lines, "testproc listening") {
		t.Errorf("ring buffer missing process output; got:\n%s", lines)
	}
	waitForFileContains(t, s.LogPath(), "testproc listening")

	pid := snap.PID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := s.Snapshot().State; got != StateStopped {
		t.Errorf("state after stop = %q, want %q", got, StateStopped)
	}
	waitProcessGone(t, pid)
	if exit := s.Snapshot().LastExit; exit == "" {
		t.Error("LastExit should be recorded after stop")
	}
}

// waitForFileContains polls a file until it contains want (the pump writes
// asynchronously).
func waitForFileContains(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("log file %s missing %q; got:\n%s", path, want, data)
}

func TestStartHTTPReadiness(t *testing.T) {
	p := helperProject(t, testproc.ModeHTTP)
	p.ReadinessStrategy = config.ReadinessHTTP
	p.ReadinessURL = "http://127.0.0.1:" + strconv.Itoa(p.ApplicationPort) + "/"
	s := newTestSupervisor(t, p)

	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	if got := s.Snapshot().State; got != StateRunning {
		t.Errorf("state = %q, want %q", got, StateRunning)
	}
}

func TestEnsureStartedSharesOneStartup(t *testing.T) {
	s := newTestSupervisor(t, helperProject(t, testproc.ModeListen))

	first := s.EnsureStarted()
	second := s.EnsureStarted() // while starting: must join, not respawn

	if err := awaitStartup(t, first); err != nil {
		t.Fatalf("first EnsureStarted: %v", err)
	}
	if err := awaitStartup(t, second); err != nil {
		t.Fatalf("second EnsureStarted: %v", err)
	}
	pid := s.Snapshot().PID

	// Already running: immediate success, same process.
	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted while running: %v", err)
	}
	if got := s.Snapshot().PID; got != pid {
		t.Errorf("PID changed %d -> %d; EnsureStarted must never respawn a running project", pid, got)
	}
}

func TestStartupFailureOnEarlyExit(t *testing.T) {
	s := newTestSupervisor(t, shellProject(t, "echo boom >&2; exit 7"))

	err := awaitStartup(t, s.EnsureStarted())

	if err == nil {
		t.Fatal("EnsureStarted should fail when the process exits before readiness")
	}
	if !strings.Contains(err.Error(), "logs") {
		t.Errorf("error should point at the logs command; got: %v", err)
	}
	snap := s.Snapshot()
	if snap.State != StateFailed {
		t.Errorf("state = %q, want %q", snap.State, StateFailed)
	}
	if snap.LastExit != "exit status 7" {
		t.Errorf("LastExit = %q, want %q", snap.LastExit, "exit status 7")
	}
	if lines := strings.Join(s.Logs(0), "\n"); !strings.Contains(lines, "boom") {
		t.Errorf("logs should contain the process's stderr; got:\n%s", lines)
	}
}

func TestStartupFailureOnReadinessTimeout(t *testing.T) {
	p := shellProject(t, "sleep 60") // never listens
	p.StartupTimeoutSeconds = 1
	s := newTestSupervisor(t, p)

	ch := s.EnsureStarted()
	pid := s.Snapshot().PID
	if pid <= 0 {
		t.Fatal("expected a live pid while starting")
	}
	err := awaitStartup(t, ch)

	if err == nil {
		t.Fatal("EnsureStarted should fail on readiness timeout")
	}
	if !strings.Contains(err.Error(), "not ready within") {
		t.Errorf("error should explain the timeout; got: %v", err)
	}
	if got := s.Snapshot().State; got != StateFailed {
		t.Errorf("state = %q, want %q", got, StateFailed)
	}
	waitProcessGone(t, pid) // the timed-out process group must be killed
}

func TestGracefulStopKillsWholeProcessGroup(t *testing.T) {
	childPidfile := filepath.Join(t.TempDir(), "child.pid")
	p := helperProject(t, testproc.ModeParent)
	p.Env[testproc.EnvChildPidfile] = childPidfile
	s := newTestSupervisor(t, p)

	// Readiness comes from the *child* listener: proof the group has depth.
	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	parentPID := s.Snapshot().PID
	childPID := readPidfile(t, childPidfile)

	begin := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Errorf("graceful stop took %s; SIGTERM to the group should end it well before the %ds force-kill",
			elapsed, p.ShutdownTimeoutSeconds)
	}
	if got := s.Snapshot().State; got != StateStopped {
		t.Errorf("state = %q, want %q", got, StateStopped)
	}
	waitProcessGone(t, parentPID)
	waitProcessGone(t, childPID)
}

func TestForceKillAfterShutdownTimeout(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "stubborn.pid")
	p := helperProject(t, testproc.ModeStubborn) // ignores SIGTERM
	p.Env[testproc.EnvPidfile] = pidfile
	p.ShutdownTimeoutSeconds = 1
	s := newTestSupervisor(t, p)

	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	pid := s.Snapshot().PID
	// The stubborn helper's own pid: on shells that exec the command it IS
	// the direct child; on shells that keep an intermediate process it is a
	// grandchild that outlives the direct child.
	stubbornPID := readPidfile(t, pidfile)

	begin := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	elapsed := time.Since(begin)
	if elapsed < time.Second {
		t.Errorf("stop finished in %s; the graceful window is 1s, so SIGKILL must not fire early", elapsed)
	}
	if got := s.Snapshot().State; got != StateStopped {
		t.Errorf("state = %q, want %q", got, StateStopped)
	}
	waitProcessGone(t, pid)
	waitProcessGone(t, stubbornPID) // SIGKILL escalation must reach the SIGTERM-ignoring process
	// Direct child SIGKILLed ("signal: killed") or an intermediate shell died
	// on SIGTERM and the surviving group was force-killed — either way the
	// summary must record the SIGKILL escalation.
	if exit := s.Snapshot().LastExit; !strings.Contains(exit, "killed") {
		t.Errorf("LastExit = %q, want it to record the SIGKILL escalation", exit)
	}
}

// TestForceKillReachesStubbornGrandchildOfExitedParent reproduces, on every
// platform, the topology dash creates on Linux CI: the direct child dies on
// the group's graceful signal almost immediately, while a SIGTERM-ignoring
// grandchild survives. Stop must not declare the project stopped until the
// whole group is gone, must hold the SIGKILL until the graceful window
// elapses, and must then kill the stubborn grandchild.
func TestForceKillReachesStubbornGrandchildOfExitedParent(t *testing.T) {
	childPidfile := filepath.Join(t.TempDir(), "stubborn-child.pid")
	p := helperProject(t, testproc.ModeParent) // dies on SIGTERM (no handler)
	p.Env[testproc.EnvChildMode] = testproc.ModeStubborn
	p.Env[testproc.EnvChildPidfile] = childPidfile
	p.ShutdownTimeoutSeconds = 1
	s := newTestSupervisor(t, p)

	// Readiness comes from the stubborn grandchild's listener.
	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	parentPID := s.Snapshot().PID
	childPID := readPidfile(t, childPidfile)

	begin := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	elapsed := time.Since(begin)
	if elapsed < time.Second {
		t.Errorf("stop finished in %s; the direct child's quick death must not end the stop while the grandchild survives", elapsed)
	}
	if got := s.Snapshot().State; got != StateStopped {
		t.Errorf("state = %q, want %q", got, StateStopped)
	}
	waitProcessGone(t, parentPID)
	waitProcessGone(t, childPID) // the stubborn grandchild must have been SIGKILLed
	if exit := s.Snapshot().LastExit; !strings.Contains(exit, "killed") {
		t.Errorf("LastExit = %q, want it to record the SIGKILL escalation", exit)
	}
}

func TestExternalKillIsDetectedAndRecoverable(t *testing.T) {
	s := newTestSupervisor(t, helperProject(t, testproc.ModeListen))
	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	pid := s.Snapshot().PID

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}

	waitForState(t, s, StateFailed)
	snap := s.Snapshot()
	if !strings.Contains(snap.LastExit, "killed") {
		t.Errorf("LastExit = %q, want it to record the kill signal", snap.LastExit)
	}
	if snap.LastError == "" {
		t.Error("LastError should explain the unexpected exit")
	}
	if snap.PID != 0 {
		t.Errorf("PID = %d after exit, want 0", snap.PID)
	}

	// A failed project can be started again.
	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted after failure: %v", err)
	}
	if got := s.Snapshot().State; got != StateRunning {
		t.Errorf("state = %q, want %q", got, StateRunning)
	}
}

func TestCleanExitWhileRunningBecomesStopped(t *testing.T) {
	p := helperProject(t, testproc.ModeListenExit)
	p.Env[testproc.EnvLinger] = "300ms"
	s := newTestSupervisor(t, p)

	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}

	waitForState(t, s, StateStopped)
	if exit := s.Snapshot().LastExit; exit != "exit status 0" {
		t.Errorf("LastExit = %q, want %q", exit, "exit status 0")
	}
	if lastErr := s.Snapshot().LastError; lastErr != "" {
		t.Errorf("LastError = %q, want empty after a clean exit", lastErr)
	}
}

func TestStopWhileStartingFailsTheStartWaiters(t *testing.T) {
	p := shellProject(t, "sleep 60")
	p.StartupTimeoutSeconds = 60
	s := newTestSupervisor(t, p)

	ch := s.EnsureStarted()
	if got := s.Snapshot().State; got != StateStarting {
		t.Fatalf("state = %q, want %q", got, StateStarting)
	}
	pid := s.Snapshot().PID

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	err := awaitStartup(t, ch)
	if err == nil || !strings.Contains(err.Error(), "stopped while starting") {
		t.Errorf("EnsureStarted outcome = %v, want a stopped-while-starting error", err)
	}
	if got := s.Snapshot().State; got != StateStopped {
		t.Errorf("state = %q, want %q", got, StateStopped)
	}
	waitProcessGone(t, pid)
}

func TestStopWhenNeverStartedIsNoOp(t *testing.T) {
	s := newTestSupervisor(t, helperProject(t, testproc.ModeListen))

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop on a stopped project: %v", err)
	}
	if got := s.Snapshot().State; got != StateStopped {
		t.Errorf("state = %q, want %q", got, StateStopped)
	}
}

func TestRestartSpawnsANewProcess(t *testing.T) {
	s := newTestSupervisor(t, helperProject(t, testproc.ModeListen))
	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	firstPID := s.Snapshot().PID

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.Restart(ctx); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	snap := s.Snapshot()
	if snap.State != StateRunning {
		t.Errorf("state = %q, want %q", snap.State, StateRunning)
	}
	if snap.PID == firstPID {
		t.Errorf("PID = %d after restart, want a new process", snap.PID)
	}
	waitProcessGone(t, firstPID)
}

func TestSpawnFailureFailsImmediately(t *testing.T) {
	p := shellProject(t, "true")
	p.WorkingDirectory = filepath.Join(t.TempDir(), "does-not-exist")
	s := newTestSupervisor(t, p)

	err := awaitStartup(t, s.EnsureStarted())

	if err == nil {
		t.Fatal("EnsureStarted should fail when the working directory is missing")
	}
	if got := s.Snapshot().State; got != StateFailed {
		t.Errorf("state = %q, want %q", got, StateFailed)
	}
	if s.Snapshot().LastError == "" {
		t.Error("LastError should be set after a spawn failure")
	}
}

func TestBackoffDelaySchedule(t *testing.T) {
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, w := range want {
		if got := backoffDelay(i + 1); got != w {
			t.Errorf("backoffDelay(%d) = %s, want %s", i+1, got, w)
		}
	}
	if got := backoffDelay(0); got != 0 {
		t.Errorf("backoffDelay(0) = %s, want 0", got)
	}
	if got := backoffDelay(1000); got != backoffMax {
		t.Errorf("backoffDelay(1000) = %s, want the %s cap (no overflow)", got, backoffMax)
	}
}

// waitForSpawnCount polls the ring buffer until the marker line appears
// exactly want times (each spawn of the crashing command prints it once).
func waitForSpawnCount(t *testing.T, s *Supervisor, marker string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	count := func() int {
		n := 0
		for _, line := range s.Logs(0) {
			if strings.Contains(line, marker) {
				n++
			}
		}
		return n
	}
	for time.Now().Before(deadline) {
		if count() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("spawn count = %d, want %d", count(), want)
}

func TestOnDemandStartBackoffAndManualReset(t *testing.T) {
	s := newTestSupervisor(t, shellProject(t, "echo spawn-marker; exit 1"))

	// First on-demand start spawns and fails; the outcome is delivered only
	// after the pump captured the process output.
	err := awaitStartup(t, s.EnsureStartedOnDemand())
	if err == nil {
		t.Fatal("EnsureStartedOnDemand on a crashing command should fail")
	}
	var be *BackoffError
	if errors.As(err, &be) {
		t.Fatalf("first failure should be the startup error, not backoff; got %v", err)
	}
	waitForSpawnCount(t, s, "spawn-marker", 1)
	if s.Snapshot().NextRetryAt.IsZero() {
		t.Error("NextRetryAt should be scheduled after a failed start")
	}

	// On-demand starts inside the backoff window are refused without a
	// respawn.
	for i := 0; i < 3; i++ {
		err := awaitStartup(t, s.EnsureStartedOnDemand())
		if !errors.As(err, &be) {
			t.Fatalf("attempt %d during backoff: error = %v, want a *BackoffError", i, err)
		}
	}
	waitForSpawnCount(t, s, "spawn-marker", 1)

	// A manual start bypasses and resets the backoff: it respawns
	// immediately (and fails again).
	err = awaitStartup(t, s.EnsureStarted())
	if err == nil {
		t.Fatal("manual EnsureStarted on a crashing command should fail")
	}
	if errors.As(err, &be) {
		t.Fatalf("manual start must bypass backoff; got %v", err)
	}
	waitForSpawnCount(t, s, "spawn-marker", 2)

	// The reset put us back at the first backoff step (1s); inside it,
	// on-demand starts are again refused ...
	err = awaitStartup(t, s.EnsureStartedOnDemand())
	if !errors.As(err, &be) {
		t.Fatalf("error after manual failure = %v, want a *BackoffError", err)
	}
	waitForSpawnCount(t, s, "spawn-marker", 2)

	// ... and once the window elapses, an on-demand start retries.
	if wait := time.Until(be.RetryAt) + 100*time.Millisecond; wait > 0 {
		time.Sleep(wait)
	}
	err = awaitStartup(t, s.EnsureStartedOnDemand())
	if err == nil || errors.As(err, &be) {
		t.Fatalf("post-backoff start should respawn and report the real failure; got %v", err)
	}
	waitForSpawnCount(t, s, "spawn-marker", 3)
}

func TestBackoffResetOnSuccessfulStart(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "helper.pid")
	p := helperProject(t, testproc.ModeListen)
	p.Env[testproc.EnvPidfile] = pidfile
	s := newTestSupervisor(t, p)

	if err := awaitStartup(t, s.EnsureStartedOnDemand()); err != nil {
		t.Fatalf("EnsureStartedOnDemand: %v", err)
	}
	if got := s.Snapshot().NextRetryAt; !got.IsZero() {
		t.Errorf("NextRetryAt = %v after a successful start, want zero", got)
	}

	// After an unexpected external kill, the next request-triggered start is
	// NOT throttled: no startup failure has occurred.
	pid := s.Snapshot().PID
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitForState(t, s, StateFailed)
	if err := awaitStartup(t, s.EnsureStartedOnDemand()); err != nil {
		t.Fatalf("EnsureStartedOnDemand after external kill: %v", err)
	}
}

func TestStateIsLockFreeMirrorOfLifecycle(t *testing.T) {
	s := newTestSupervisor(t, helperProject(t, testproc.ModeListen))
	if got := s.State(); got != StateStopped {
		t.Fatalf("State() = %q, want %q", got, StateStopped)
	}
	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	if got := s.State(); got != StateRunning {
		t.Errorf("State() = %q, want %q", got, StateRunning)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := s.State(); got != StateStopped {
		t.Errorf("State() = %q, want %q", got, StateStopped)
	}
}

func TestStartupFailureOutcomeIncludesFinalOutput(t *testing.T) {
	// The failure outcome must not race the output pump: by the time
	// EnsureStarted reports the failure, the crash's log lines are readable.
	s := newTestSupervisor(t, shellProject(t, "echo first-line; echo last-words >&2; exit 9"))

	if err := awaitStartup(t, s.EnsureStarted()); err == nil {
		t.Fatal("EnsureStarted should fail")
	}

	lines := strings.Join(s.Logs(0), "\n")
	for _, want := range []string{"first-line", "last-words"} {
		if !strings.Contains(lines, want) {
			t.Errorf("logs at failure time missing %q; got:\n%s", want, lines)
		}
	}
}

func TestProjectEnvReachesProcess(t *testing.T) {
	p := shellProject(t, `echo "value=$HW_TEST_VALUE"; exit 0`)
	p.Env = map[string]string{"HW_TEST_VALUE": "hello-env"}
	s := newTestSupervisor(t, p)

	err := awaitStartup(t, s.EnsureStarted()) // exits 0 before readiness -> failed
	if err == nil {
		t.Fatal("expected startup failure from immediate exit")
	}
	if lines := strings.Join(s.Logs(0), "\n"); !strings.Contains(lines, "value=hello-env") {
		t.Errorf("configured env did not reach the process; logs:\n%s", lines)
	}
}

// TestRetireRefusesFurtherStartsButStillStops: a retired supervisor (its
// project removed or replaced by a config reload) never spawns again — not
// via the manual path, not via the on-demand path — while Stop still
// terminates the process it was running.
func TestRetireRefusesFurtherStartsButStillStops(t *testing.T) {
	s := newTestSupervisor(t, helperProject(t, testproc.ModeListen))
	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	pid := s.Snapshot().PID

	s.Retire()

	// Running: a start request is still satisfied by the running process.
	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Errorf("EnsureStarted on a running retired supervisor = %v, want nil", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after Retire: %v", err)
	}
	waitProcessGone(t, pid)
	if got := s.State(); got != StateStopped {
		t.Fatalf("state after stop = %q, want %q", got, StateStopped)
	}

	for name, start := range map[string]func() <-chan error{
		"manual":    s.EnsureStarted,
		"on-demand": s.EnsureStartedOnDemand,
	} {
		err := awaitStartup(t, start())
		if err == nil || !strings.Contains(err.Error(), "config reload") {
			t.Errorf("%s start after Retire = %v, want a refusal mentioning the config reload", name, err)
		}
	}
	if got := s.State(); got != StateStopped {
		t.Errorf("state after refused starts = %q, want %q (nothing spawned)", got, StateStopped)
	}
}

// TestStopAsyncReturnsOnceStopping: StopAsync hands back control the moment
// the project is stopping — a stubborn process that ignores SIGTERM keeps
// the stop in progress for the full shutdown timeout — and its channel
// closes only once the process group is really gone.
func TestStopAsyncReturnsOnceStopping(t *testing.T) {
	p := helperProject(t, testproc.ModeStubborn)
	p.ShutdownTimeoutSeconds = 1
	s := newTestSupervisor(t, p)
	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := s.Snapshot().PID

	began := time.Now()
	done := s.StopAsync()
	if took := time.Since(began); took > 500*time.Millisecond {
		t.Errorf("StopAsync took %s, want an immediate return", took)
	}
	if got := s.State(); got != StateStopping {
		t.Errorf("state right after StopAsync = %q, want %q", got, StateStopping)
	}
	select {
	case <-done:
		t.Fatal("stop reported finished while the stubborn process still had its graceful window")
	default:
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("stop never finished")
	}
	if got := s.State(); got != StateStopped {
		t.Errorf("state after the stop finished = %q, want %q", got, StateStopped)
	}
	waitProcessGone(t, pid)
	// A second StopAsync on a stopped project is a closed channel.
	select {
	case <-s.StopAsync():
	case <-time.After(time.Second):
		t.Error("StopAsync on a stopped project should return a closed channel")
	}
}

// writeEnvFile writes an env file with the given mode, creating its parent
// directories, and fails the test if it cannot.
func writeEnvFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is masked by the umask; force the mode we asked for,
	// so a 0600 file really is 0600 and a deliberately loose one really is
	// readable by other users under any umask the suite runs with.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// envReportingCommand wraps the project's helper command in a shell line
// that records the child's view of the named variables, one per line, in
// "env.txt" inside the working directory before exec'ing the helper. It is
// how these tests observe an environment that must never be printed
// anywhere else.
func envReportingCommand(p *config.Project, names ...string) string {
	var b strings.Builder
	for _, name := range names {
		redirect := ">>"
		if b.Len() == 0 {
			redirect = ">"
		}
		b.WriteString("printenv " + name + " " + redirect + " env.txt; ")
	}
	return b.String() + "exec " + p.Command
}

// readEnvReport waits for envReportingCommand's file and returns its lines.
func readEnvReport(t *testing.T, dir string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(dir, "env.txt"))
		if err == nil && len(data) > 0 {
			return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the process never wrote its environment report")
	return nil
}

// TestEnvFilesReachProcessInPrecedenceOrder: a shared absolute env file and
// a per-worktree relative one both reach the child — the helper itself is
// configured entirely from the shared file — with the later file beating
// the earlier one and the inline env map beating both.
func TestEnvFilesReachProcessInPrecedenceOrder(t *testing.T) {
	p := helperProject(t, testproc.ModeListen)
	shared := filepath.Join(t.TempDir(), "preview.env")
	writeEnvFile(t, shared, "# shared by every preview\n"+
		"export "+testproc.EnvMode+"="+testproc.ModeListen+"\n"+
		testproc.EnvPort+"="+strconv.Itoa(p.ApplicationPort)+"\n"+
		"DB_LOGIN='postgres://app:hunter2@db/app'\n"+
		"ENVIRONMENT=shared\n"+
		"INLINE_WINS=from-file\n", 0o600)
	writeEnvFile(t, filepath.Join(p.WorkingDirectory, ".env.preview"),
		"ENVIRONMENT=per-worktree\n", 0o600)
	p.EnvFile = config.EnvFiles{shared, ".env.preview"}
	p.Env = map[string]string{"INLINE_WINS": "from-inline"}
	p.Command = envReportingCommand(p, "DB_LOGIN", "ENVIRONMENT", "INLINE_WINS")
	s := newTestSupervisor(t, p)

	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}

	want := []string{"postgres://app:hunter2@db/app", "per-worktree", "from-inline"}
	got := readEnvReport(t, p.WorkingDirectory)
	if len(got) != len(want) {
		t.Fatalf("child environment report = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("report line %d = %q, want %q", i+1, got[i], want[i])
		}
	}
}

// TestMissingRelativeEnvFileIsSkipped: a worktree without the per-worktree
// file is the normal case, so the start must succeed.
func TestMissingRelativeEnvFileIsSkipped(t *testing.T) {
	p := helperProject(t, testproc.ModeListen)
	p.EnvFile = config.EnvFiles{".env.preview"}
	s := newTestSupervisor(t, p)

	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted with a missing optional env file: %v", err)
	}
	if got := s.Snapshot().State; got != StateRunning {
		t.Errorf("state = %q, want %q", got, StateRunning)
	}
}

// TestMissingAbsoluteEnvFileFailsTheStart: a shared secrets file that has
// vanished is loud — the start fails naming the path, nothing is spawned,
// and the supervisor lands in failed like any other startup failure.
func TestMissingAbsoluteEnvFileFailsTheStart(t *testing.T) {
	p := helperProject(t, testproc.ModeListen)
	missing := filepath.Join(t.TempDir(), "preview.env")
	p.EnvFile = config.EnvFiles{missing}
	p.Command = "touch spawned; " + p.Command
	s := newTestSupervisor(t, p)

	err := awaitStartup(t, s.EnsureStarted())

	if err == nil {
		t.Fatal("EnsureStarted should fail when a required env file is missing")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %q, want it to name %q", err, missing)
	}
	snap := s.Snapshot()
	if snap.State != StateFailed {
		t.Errorf("state = %q, want %q", snap.State, StateFailed)
	}
	if snap.LastError == "" {
		t.Error("LastError should be set after a failed start")
	}
	if snap.PID != 0 {
		t.Errorf("PID = %d, want no process", snap.PID)
	}
	if _, err := os.Stat(filepath.Join(p.WorkingDirectory, "spawned")); !os.IsNotExist(err) {
		t.Error("the command ran although the environment could not be built")
	}
}

// TestMalformedEnvFileFailsTheStartWithoutItsContent: a bad line is
// reported by number only. The line here carries a password, which must
// appear in neither the error nor the recorded failure.
func TestMalformedEnvFileFailsTheStartWithoutItsContent(t *testing.T) {
	p := helperProject(t, testproc.ModeListen)
	bad := filepath.Join(t.TempDir(), "preview.env")
	writeEnvFile(t, bad, "ENVIRONMENT=preview\nFOO hunter2\n", 0o600)
	p.EnvFile = config.EnvFiles{bad}
	s := newTestSupervisor(t, p)

	err := awaitStartup(t, s.EnsureStarted())

	if err == nil {
		t.Fatal("EnsureStarted should fail on a malformed env file")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error = %q, want it to name the bad line", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error = %q; it must not quote the line's content", err)
	}
	if got := s.Snapshot().LastError; strings.Contains(got, "hunter2") {
		t.Errorf("LastError = %q; it must not quote the line's content", got)
	}
	if got := s.Snapshot().State; got != StateFailed {
		t.Errorf("state = %q, want %q", got, StateFailed)
	}
}

// TestWorldReadableEnvFileWarnsAtEveryStart: a chmod mistake on a secrets
// file is visible in the daemon log, every time, with the path and mode but
// no content — and it never refuses the start.
func TestWorldReadableEnvFileWarnsAtEveryStart(t *testing.T) {
	p := helperProject(t, testproc.ModeListen)
	loose := filepath.Join(t.TempDir(), "preview.env")
	writeEnvFile(t, loose, "DB_LOGIN=hunter2\n", 0o644)
	p.EnvFile = config.EnvFiles{loose}
	logs := &lockedBuffer{}
	s := NewSupervisor(p, t.TempDir(), log.New(logs, "", 0))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.Stop(ctx); err != nil {
			t.Errorf("cleanup stop: %v", err)
		}
	})

	for attempt := 1; attempt <= 2; attempt++ {
		if err := awaitStartup(t, s.EnsureStarted()); err != nil {
			t.Fatalf("EnsureStarted (attempt %d): %v", attempt, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := s.Stop(ctx)
		cancel()
		if err != nil {
			t.Fatalf("stop (attempt %d): %v", attempt, err)
		}
	}

	out := logs.String()
	if got := strings.Count(out, "readable by other users"); got != 2 {
		t.Errorf("permission warning logged %d times, want once per start; log:\n%s", got, out)
	}
	if !strings.Contains(out, loose) {
		t.Errorf("warning does not name the file; log:\n%s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("the log must never contain an env value; log:\n%s", out)
	}
}

// lockedBuffer collects log output from the supervisor's goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
