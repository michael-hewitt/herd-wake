package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// TestMain lets the test binary double as the spawned child processes.
func TestMain(m *testing.M) {
	testproc.Main() // exits the process when running as a helper
	os.Exit(m.Run())
}

// helperConfig builds a one-project config whose command re-invokes the test
// binary as a TCP listener on the application port.
func helperConfig(t *testing.T) *config.Config {
	t.Helper()
	command, err := testproc.Command()
	if err != nil {
		t.Fatal(err)
	}
	applicationPort := freePort(t)
	return &config.Config{Projects: map[string]*config.Project{
		"dashboard": {
			Name:                   "dashboard",
			PublicURL:              "https://dashboard.test",
			SupervisorPort:         freePort(t),
			ApplicationPort:        applicationPort,
			ListenHost:             "127.0.0.1",
			WorkingDirectory:       t.TempDir(),
			Command:                command,
			ReadinessStrategy:      config.ReadinessTCP,
			StartupTimeoutSeconds:  10,
			ShutdownSignal:         "SIGTERM",
			ShutdownTimeoutSeconds: 5,
			LogRetentionDays:       7,
			Env: map[string]string{
				testproc.EnvMode: testproc.ModeListen,
				testproc.EnvPort: strconv.Itoa(applicationPort),
			},
		},
	}}
}

func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists", pid)
}

func TestDaemonProjectLifecycleOverControlSocket(t *testing.T) {
	cfg := helperConfig(t)
	logDir := t.TempDir()
	socket := testSocketPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	d := New(cfg, socket, logDir, log.New(io.Discard, "", 0))
	runDone := make(chan error, 1)
	go func() {
		runDone <- d.Run(ctx)
		close(runDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			t.Error("daemon did not shut down")
		}
	})
	waitForDaemon(t, socket, runDone)
	client := control.NewClient(socket)
	callCtx, callCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer callCancel()

	// Start: stopped -> running, with a real PID.
	started, err := client.StartProject(callCtx, "dashboard")
	if err != nil {
		t.Fatalf("StartProject: %v", err)
	}
	if started.State != StateRunning || started.PID <= 0 {
		t.Fatalf("StartProject status = %+v, want running with a pid", started)
	}

	// Status reflects the live process.
	status, err := client.Status(callCtx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	p := status.Projects[0]
	if p.State != StateRunning || p.PID != started.PID || p.UptimeSeconds < 0 {
		t.Errorf("status project = %+v, want running with pid %d", p, started.PID)
	}

	// Logs: ring-buffer lines plus the on-disk path.
	logs, err := client.Logs(callCtx, "dashboard", 0)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if joined := strings.Join(logs.Lines, "\n"); !strings.Contains(joined, "testproc listening") {
		t.Errorf("logs missing process output; got:\n%s", joined)
	}
	if want := filepath.Join(logDir, "dashboard.log"); logs.LogFile != want {
		t.Errorf("LogFile = %q, want %q", logs.LogFile, want)
	}

	// Restart: a fresh process.
	restarted, err := client.RestartProject(callCtx, "dashboard")
	if err != nil {
		t.Fatalf("RestartProject: %v", err)
	}
	if restarted.State != StateRunning || restarted.PID == started.PID || restarted.PID <= 0 {
		t.Errorf("RestartProject status = %+v, want running with a new pid (old %d)", restarted, started.PID)
	}
	waitProcessGone(t, started.PID)

	// Stop: running -> stopped, process group gone.
	stopped, err := client.StopProject(callCtx, "dashboard")
	if err != nil {
		t.Fatalf("StopProject: %v", err)
	}
	if stopped.State != StateStopped {
		t.Errorf("StopProject status = %+v, want stopped", stopped)
	}
	waitProcessGone(t, restarted.PID)
}

func TestDaemonUnknownProjectIsRejected(t *testing.T) {
	socket, _, _ := startDaemon(t, testConfig(freePort(t), freePort(t)))
	client := control.NewClient(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.StartProject(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "unknown project") {
		t.Errorf("StartProject(nope) error = %v, want unknown-project", err)
	}
	if _, err := client.Logs(ctx, "nope", 0); err == nil || !strings.Contains(err.Error(), "unknown project") {
		t.Errorf("Logs(nope) error = %v, want unknown-project", err)
	}
	var apiErr *control.APIError
	_, err := client.StopProject(ctx, "nope")
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
		t.Errorf("StopProject(nope) error = %v, want a 404 APIError", err)
	}
}

func TestDaemonShutdownTerminatesSupervisedProcesses(t *testing.T) {
	cfg := helperConfig(t)
	socket := testSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	d := New(cfg, socket, t.TempDir(), log.New(io.Discard, "", 0))
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	defer cancel()
	waitForDaemon(t, socket, runDone)

	callCtx, callCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer callCancel()
	started, err := control.NewClient(socket).StartProject(callCtx, "dashboard")
	if err != nil {
		t.Fatalf("StartProject: %v", err)
	}

	cancel() // daemon shutdown (the SIGTERM path in the CLI)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not shut down")
	}

	// The supervised process group must not be orphaned.
	waitProcessGone(t, started.PID)
}
