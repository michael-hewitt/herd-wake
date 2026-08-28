// Test harness for the e2e suite: guard, binary build, fixture install,
// daemon subprocess management, control-API polling, and process counting.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/control"
)

// envGuard must be "1" for the suite to run; anything else skips every test
// so the ordinary `go test ./...` inner loop stays fast.
const envGuard = "HW_E2E"

// Shared, set up once in TestMain when the guard is on.
var (
	binPath    string // the compiled herd-wake binary
	repoRoot   string // module root (parent of this package)
	fixtureDir string // testdata/vite-fixture
)

// e2eEnabled reports whether the guard is on.
func e2eEnabled() bool { return os.Getenv(envGuard) == "1" }

// requireE2E skips the test unless HW_E2E=1 (and not -short).
func requireE2E(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping e2e test in -short mode")
	}
	if !e2eEnabled() {
		t.Skipf("skipping e2e test: set %s=1 to run the acceptance suite", envGuard)
	}
}

func TestMain(m *testing.M) {
	if !e2eEnabled() {
		os.Exit(m.Run()) // every test skips itself
	}
	code := func() int {
		var err error
		repoRoot, err = filepath.Abs("..")
		if err != nil {
			fmt.Fprintln(os.Stderr, "e2e: resolve repo root:", err)
			return 1
		}
		fixtureDir = filepath.Join(repoRoot, "testdata", "vite-fixture")

		binDir, err := os.MkdirTemp("", "herd-wake-e2e")
		if err != nil {
			fmt.Fprintln(os.Stderr, "e2e:", err)
			return 1
		}
		defer os.RemoveAll(binDir) //nolint:errcheck // best-effort cleanup
		binPath = filepath.Join(binDir, "herd-wake")

		// Build the daemon with the race detector so e2e traffic exercises it.
		build := exec.Command("go", "build", "-race", "-o", binPath, "./cmd/herd-wake")
		build.Dir = repoRoot
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: build herd-wake:", err)
			return 1
		}

		if err := ensureFixtureDeps(); err != nil {
			fmt.Fprintln(os.Stderr, "e2e:", err)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

// ensureFixtureDeps installs the Vite fixture's pinned dependencies when
// node_modules is missing (locally on first run; CI installs them itself for
// caching, which this then detects and skips).
func ensureFixtureDeps() error {
	if _, err := os.Stat(filepath.Join(fixtureDir, "node_modules", ".bin", "vite")); err == nil {
		return nil
	}
	fmt.Fprintln(os.Stderr, "e2e: installing fixture dependencies (npm ci in testdata/vite-fixture)")
	cmd := exec.Command("npm", "ci")
	cmd.Dir = fixtureDir
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm ci in %s: %w", fixtureDir, err)
	}
	return nil
}

// lockedBuffer is a bytes.Buffer safe for the daemon subprocess goroutine to
// write while a failing test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
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

// daemon is one running `herd-wake start` subprocess plus everything needed
// to talk to it.
type daemon struct {
	t          *testing.T
	cmd        *exec.Cmd
	socket     string
	configPath string
	output     *lockedBuffer
	waitErr    chan error
	stopped    bool
}

// startDaemon writes projectsYAML (the body under `projects:`) to a config
// file, starts the herd-wake daemon on a private socket and log dir, and
// waits until its control API answers. The daemon is stopped via t.Cleanup.
func startDaemon(t *testing.T, projectsYAML string) *daemon {
	t.Helper()

	// The unix-socket path must stay under the ~104-byte macOS limit, so use
	// a short system temp dir rather than t.TempDir().
	dir, err := os.MkdirTemp("", "hw-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) }) //nolint:errcheck // best-effort cleanup

	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("projects:\n"+projectsYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "hw.sock")

	d := &daemon{
		t:          t,
		socket:     socket,
		configPath: configPath,
		output:     &lockedBuffer{},
		waitErr:    make(chan error, 1),
	}
	d.cmd = exec.Command(binPath, "start",
		"--config", configPath,
		"--socket", socket,
		"--log-dir", filepath.Join(dir, "logs"))
	d.cmd.Stdout = d.output
	d.cmd.Stderr = d.output
	if err := d.cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	go func() { d.waitErr <- d.cmd.Wait() }()

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon output:\n%s", d.output.String())
		}
		_ = d.stop() // exit status only matters where a test asserts it
	})

	// Wait for the control API to answer.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := d.tryStatus(); err == nil {
			return d
		}
		select {
		case waitErr := <-d.waitErr:
			d.stopped = true
			t.Fatalf("daemon exited during startup: %v\noutput:\n%s", waitErr, d.output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon control API not answering after 30s\noutput:\n%s", d.output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stop terminates the daemon gracefully (SIGTERM, SIGKILL after 30s) and
// waits for it to exit. It returns the daemon's Wait error and is idempotent.
func (d *daemon) stop() error {
	if d.stopped {
		return nil
	}
	d.stopped = true
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-d.waitErr:
		return err
	case <-time.After(30 * time.Second):
		_ = d.cmd.Process.Kill()
		return <-d.waitErr
	}
}

// stopGracefully is stop() but failing the test if the daemon did not exit
// cleanly.
func (d *daemon) stopGracefully() {
	d.t.Helper()
	if err := d.stop(); err != nil {
		d.t.Fatalf("daemon did not exit cleanly on SIGTERM: %v\noutput:\n%s", err, d.output.String())
	}
}

// controlClient returns an HTTP client that dials the daemon's unix control
// socket regardless of the request URL's host.
func (d *daemon) controlClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", d.socket)
			},
		},
	}
}

// tryStatus fetches GET /v1/status from the control API.
func (d *daemon) tryStatus() (*control.StatusResponse, error) {
	resp, err := d.controlClient().Get("http://herd-wake/v1/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response fully read below
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status: HTTP %d", resp.StatusCode)
	}
	var status control.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}
	return &status, nil
}

// status fetches the daemon status, failing the test on error.
func (d *daemon) status() *control.StatusResponse {
	d.t.Helper()
	status, err := d.tryStatus()
	if err != nil {
		d.t.Fatalf("daemon status: %v", err)
	}
	return status
}

// projectStatus returns the named project's entry from the daemon status.
func (d *daemon) projectStatus(name string) control.ProjectStatus {
	d.t.Helper()
	for _, p := range d.status().Projects {
		if p.Name == name {
			return p
		}
	}
	d.t.Fatalf("project %q not in daemon status", name)
	return control.ProjectStatus{}
}

// waitForState polls until the named project reaches the wanted state.
func (d *daemon) waitForState(name, state string, timeout time.Duration) {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := d.projectStatus(name).State; got == state {
			return
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("project %q did not reach state %q within %s (still %q)",
				name, state, timeout, d.projectStatus(name).State)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// cli runs a herd-wake CLI command against this daemon's control socket and
// returns its combined output. Flags must precede positional arguments, so
// --socket goes right after the command name.
func (d *daemon) cli(command string, args ...string) (string, error) {
	d.t.Helper()
	full := append([]string{command, "--socket", d.socket}, args...)
	cmd := exec.Command(binPath, full...) //nolint:gosec // test binary + fixed args
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// supervisorClient is a shared HTTP client for requests through supervisor
// ports; the generous timeout covers cold starts.
var supervisorClient = &http.Client{Timeout: 3 * time.Minute}

// get performs GET http://127.0.0.1:port/path through a supervisor listener
// and returns the status code and body.
func get(t *testing.T, port int, path string) (int, string) {
	t.Helper()
	resp, err := supervisorClient.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		t.Fatalf("GET :%d%s: %v", port, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // body fully read below
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET :%d%s: read body: %v", port, path, err)
	}
	return resp.StatusCode, string(body)
}

// freePorts reserves n distinct free loopback TCP ports and returns them.
// The listeners are closed before returning, so a very unlucky race with
// another local process is possible but vanishingly rare in practice.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for range n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range listeners {
		_ = ln.Close()
	}
	return ports
}

// countViteProcs counts running Vite dev-server processes started for the
// fixture on the given application port, by scanning full process command
// lines for the node_modules/.bin/vite invocation with that exact port.
func countViteProcs(t *testing.T, appPort int) int {
	t.Helper()
	out, err := exec.Command("ps", "ax", "-o", "command=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	count := 0
	portArg := fmt.Sprintf("--port %d", appPort)
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, "node_modules/.bin/vite") && strings.Contains(line, portArg) {
			count++
		}
	}
	return count
}

// viteProject renders the config entry for the Vite fixture. idleSeconds 0
// leaves the default idle timeout (15 minutes) in effect.
func viteProject(name string, supervisorPort, appPort, idleSeconds int) string {
	return fmt.Sprintf(`  %s:
    public_url: https://%s.test
    supervisor_port: %d
    application_port: %d
    working_directory: %s
    command: npm run dev -- --host 127.0.0.1 --port %d --strictPort
    startup_timeout_seconds: 120
    idle_timeout_seconds: %d
`, name, name, supervisorPort, appPort, fixtureDir, appPort, idleSeconds)
}

// nodeProject renders the config entry for a trivial Node HTTP server (a
// second, fast project for isolation tests). It writes server.js into a
// fresh temp directory.
func nodeProject(t *testing.T, name string, supervisorPort, appPort, idleSeconds int) string {
	t.Helper()
	dir := t.TempDir()
	server := `const http = require('http');
const port = Number(process.argv[2]);
http.createServer((req, res) => { res.end('hello from ` + name + ` pid=' + process.pid); })
  .listen(port, '127.0.0.1');
`
	if err := os.WriteFile(filepath.Join(dir, "server.js"), []byte(server), 0o644); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`  %s:
    public_url: https://%s.test
    supervisor_port: %d
    application_port: %d
    working_directory: %s
    command: node server.js %d
    startup_timeout_seconds: 60
    idle_timeout_seconds: %d
`, name, name, supervisorPort, appPort, dir, appPort, idleSeconds)
}

// brokenProject renders the config entry for a project whose command fails
// immediately (node is asked to run a script that does not exist).
func brokenProject(t *testing.T, name string, supervisorPort, appPort int) string {
	t.Helper()
	return fmt.Sprintf(`  %s:
    public_url: https://%s.test
    supervisor_port: %d
    application_port: %d
    working_directory: %s
    command: node this-script-does-not-exist.js
    readiness_strategy: tcp
    startup_timeout_seconds: 30
`, name, name, supervisorPort, appPort, t.TempDir())
}
