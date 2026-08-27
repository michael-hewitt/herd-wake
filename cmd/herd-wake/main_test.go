package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"version"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(version) exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.HasPrefix(out, "herd-wake ") {
		t.Errorf("run(version) stdout = %q, want prefix %q", out, "herd-wake ")
	}
	if strings.TrimSpace(strings.TrimPrefix(out, "herd-wake")) == "" {
		t.Errorf("run(version) stdout = %q, want a version string after the binary name", out)
	}
}

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run(nil, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run() exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("run() stderr = %q, want usage text", stderr.String())
	}
}

func TestRunProjectsListsConfiguredProjects(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"projects", "--config", "testdata/config.yaml"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(projects) exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"dashboard",
		"https://dashboard.test",
		"7101",
		"17101",
		"npm run dev -- --port 17101 --strictPort",
		"accounts-vite",
		"https://vite.accounts.test",
		"7102",
		"17102",
		"(always on)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run(projects) stdout missing %q; got:\n%s", want, out)
		}
	}
}

func TestRunProjectsMissingConfigFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "config.yaml")

	code := run([]string{"projects", "--config", missing}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run(projects, missing config) exit code = %d, want 1", code)
	}
	msg := stderr.String()
	if !strings.Contains(msg, missing) {
		t.Errorf("stderr should name the missing path %q; got:\n%s", missing, msg)
	}
	for _, want := range []string{"projects:", "public_url:", "supervisor_port:", "command:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr should include a sample config with %q; got:\n%s", want, msg)
		}
	}
}

func TestRunProjectsInvalidConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"projects", "--config", "testdata/invalid.yaml"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run(projects, invalid config) exit code = %d, want 1", code)
	}
	msg := stderr.String()
	if !strings.Contains(msg, `project "broken"`) {
		t.Errorf("stderr should name the offending project; got:\n%s", msg)
	}
	if !strings.Contains(msg, "command") || !strings.Contains(msg, "public_url") {
		t.Errorf("stderr should name the offending fields; got:\n%s", msg)
	}
}

func TestRunProjectsBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"projects", "--bogus"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(projects --bogus) exit code = %d, want 2", code)
	}
}

func TestUsageMentionsProjects(t *testing.T) {
	var stdout, stderr bytes.Buffer

	run(nil, &stdout, &stderr)

	if !strings.Contains(stderr.String(), "projects") {
		t.Errorf("usage should mention the projects command; got:\n%s", stderr.String())
	}
}

// testSocketPath returns a unix socket path short enough for the platform's
// sun_path limit (104 bytes on macOS).
func testSocketPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d.sock")
	if len(path) < 100 {
		return path
	}
	dir, err := os.MkdirTemp("", "hw")
	if err != nil {
		t.Fatalf("make short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
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

func TestRunStatusDaemonNotRunning(t *testing.T) {
	var stdout, stderr bytes.Buffer
	socket := testSocketPath(t)

	code := run([]string{"status", "--socket", socket}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run(status) exit code = %d, want 1 when no daemon is running", code)
	}
	msg := stderr.String()
	if !strings.Contains(msg, "daemon is not running") {
		t.Errorf("stderr should say the daemon is not running; got:\n%s", msg)
	}
	if !strings.Contains(msg, "herd-wake start") {
		t.Errorf("stderr should suggest herd-wake start; got:\n%s", msg)
	}
}

func TestRunStartMissingConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "config.yaml")

	code := run([]string{"start", "--config", missing, "--socket", testSocketPath(t)}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run(start, missing config) exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), missing) {
		t.Errorf("stderr should name the missing config path; got:\n%s", stderr.String())
	}
}

func TestRunStartAndStatusEndToEnd(t *testing.T) {
	workDir := t.TempDir()
	supervisorPort := freePort(t)
	applicationPort := freePort(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := fmt.Sprintf(`projects:
  dashboard:
    public_url: https://dashboard.test
    supervisor_port: %d
    application_port: %d
    working_directory: %s
    command: npm run dev -- --port %d --strictPort
`, supervisorPort, applicationPort, workDir, applicationPort)
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	socket := testSocketPath(t)

	var startOut, startErr bytes.Buffer
	var mu sync.Mutex // startErr is written by the daemon goroutine, read on failure
	startDone := make(chan int, 1)
	go func() {
		mu.Lock()
		defer mu.Unlock()
		startDone <- run([]string{"start", "--config", configPath, "--socket", socket}, &startOut, &startErr)
	}()

	// Wait for the daemon to answer on the control socket via the CLI.
	deadline := time.Now().Add(10 * time.Second)
	var statusOut bytes.Buffer
	for {
		var statusErr bytes.Buffer
		statusOut.Reset()
		if code := run([]string{"status", "--socket", socket}, &statusOut, &statusErr); code == 0 {
			break
		}
		select {
		case code := <-startDone:
			t.Fatalf("start exited early with code %d", code)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon never answered on the control socket")
		}
		time.Sleep(20 * time.Millisecond)
	}

	out := statusOut.String()
	for _, want := range []string{
		"herd-wake daemon running",
		"dashboard",
		"stopped",
		"https://dashboard.test",
		fmt.Sprintf("%d", supervisorPort),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q; got:\n%s", want, out)
		}
	}

	// Ctrl-C: the daemon shuts down cleanly and start exits 0.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}
	select {
	case code := <-startDone:
		if code != 0 {
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("start exit code = %d after SIGINT, want 0 (stderr:\n%s)", code, startErr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("start did not exit after SIGINT")
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Errorf("control socket %s should be removed after shutdown (stat err: %v)", socket, err)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"bogus"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(bogus) exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "bogus"`) {
		t.Errorf("run(bogus) stderr = %q, want unknown-command diagnostic", stderr.String())
	}
}
