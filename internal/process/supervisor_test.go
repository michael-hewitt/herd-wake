package process

import (
	"context"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	p := helperProject(t, testproc.ModeStubborn) // ignores SIGTERM
	p.ShutdownTimeoutSeconds = 1
	s := newTestSupervisor(t, p)

	if err := awaitStartup(t, s.EnsureStarted()); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	pid := s.Snapshot().PID

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
	if exit := s.Snapshot().LastExit; !strings.Contains(exit, "killed") {
		t.Errorf("LastExit = %q, want a SIGKILL exit", exit)
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
