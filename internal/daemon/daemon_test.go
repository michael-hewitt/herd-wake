package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
)

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

// serverPort extracts the TCP port an httptest server is listening on.
func serverPort(t *testing.T, ts *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	var n int
	if _, err := fmt.Sscanf(u.Port(), "%d", &n); err != nil {
		t.Fatalf("parse port %q: %v", u.Port(), err)
	}
	return n
}

// testConfig builds a one-project config proxying supervisorPort ->
// applicationPort.
func testConfig(supervisorPort, applicationPort int) *config.Config {
	return &config.Config{Projects: map[string]*config.Project{
		"dashboard": {
			Name:            "dashboard",
			PublicURL:       "https://dashboard.test",
			SupervisorPort:  supervisorPort,
			ApplicationPort: applicationPort,
			ListenHost:      "127.0.0.1",
		},
	}}
}

// startDaemon runs a daemon in the background and waits until its control
// socket answers. It returns the socket path, a cancel func, and a channel
// yielding Run's error after cancellation.
func startDaemon(t *testing.T, cfg *config.Config) (socket string, stop func(), done <-chan error) {
	t.Helper()
	socket = testSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	d := New(cfg, socket, t.TempDir(), log.New(io.Discard, "", 0))
	runDone := make(chan error, 1)
	go func() {
		runDone <- d.Run(ctx)
		close(runDone) // later receives (e.g. cleanup) return immediately
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			t.Error("daemon did not shut down within 10s")
		}
	})
	waitForDaemon(t, socket, runDone)
	return socket, cancel, runDone
}

// waitForDaemon polls the control socket until the daemon answers.
func waitForDaemon(t *testing.T, socket string, runDone <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-runDone:
			t.Fatalf("daemon exited during startup: %v", err)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := control.NewClient(socket).Status(ctx)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon control socket never became ready")
}

func TestDaemonProxiesEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "upstream saw %s %s host=%s", r.Method, r.URL.Path, r.Host)
	}))
	defer upstream.Close()

	supervisorPort := freePort(t)
	startDaemon(t, testConfig(supervisorPort, serverPort(t, upstream)))

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/hello", supervisorPort), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "dashboard.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through supervisor port: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	want := "upstream saw GET /hello host=dashboard.test"
	if string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

func TestDaemonUpstreamDownReturns503(t *testing.T) {
	supervisorPort := freePort(t)
	applicationPort := freePort(t) // nothing listens here
	socket, _, _ := startDaemon(t, testConfig(supervisorPort, applicationPort))

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", supervisorPort))
	if err != nil {
		t.Fatalf("request through supervisor port: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"dashboard"`) {
		t.Errorf("503 body should name the project; got:\n%s", body)
	}
	if !strings.Contains(string(body), fmt.Sprintf("127.0.0.1:%d", applicationPort)) {
		t.Errorf("503 body should name the upstream address; got:\n%s", body)
	}

	// The supervisor stays healthy: the control socket still answers.
	if _, err := control.NewClient(socket).Status(context.Background()); err != nil {
		t.Errorf("daemon stopped answering after a proxy 503: %v", err)
	}
}

func TestDaemonStatusReportsDaemonAndProjects(t *testing.T) {
	supervisorPort := freePort(t)
	applicationPort := freePort(t)
	socket, _, _ := startDaemon(t, testConfig(supervisorPort, applicationPort))

	status, err := control.NewClient(socket).Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}

	if status.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", status.PID, os.Getpid())
	}
	if status.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds = %f, want >= 0", status.UptimeSeconds)
	}
	if status.Version == "" {
		t.Error("Version should not be empty")
	}
	if len(status.Projects) != 1 {
		t.Fatalf("Projects = %+v, want exactly one", status.Projects)
	}
	p := status.Projects[0]
	if p.Name != "dashboard" || p.PublicURL != "https://dashboard.test" ||
		p.SupervisorPort != supervisorPort || p.ApplicationPort != applicationPort {
		t.Errorf("project status = %+v", p)
	}
	if p.State != StateStopped {
		t.Errorf("State = %q, want %q (static in this slice)", p.State, StateStopped)
	}
}

func TestDaemonRefusesSecondInstance(t *testing.T) {
	supervisorPort := freePort(t)
	socket, _, _ := startDaemon(t, testConfig(supervisorPort, freePort(t)))

	second := New(testConfig(freePort(t), freePort(t)), socket, t.TempDir(), log.New(io.Discard, "", 0))
	err := second.Run(context.Background())

	if err == nil {
		t.Fatal("second daemon on the same socket should refuse to run")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error = %v, want it to say a daemon is already running", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
		t.Errorf("error = %v, want it to name the running daemon's pid", err)
	}

	// The first daemon must be untouched.
	if _, err := control.NewClient(socket).Status(context.Background()); err != nil {
		t.Errorf("first daemon stopped answering after refused second start: %v", err)
	}
}

func TestDaemonCleanShutdown(t *testing.T) {
	supervisorPort := freePort(t)
	socket, stop, done := startDaemon(t, testConfig(supervisorPort, freePort(t)))

	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil on clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not shut down")
	}

	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Errorf("control socket file %s should be removed on shutdown (stat err: %v)", socket, err)
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", supervisorPort), time.Second)
	if err == nil {
		_ = conn.Close()
		t.Errorf("supervisor port %d still accepts connections after shutdown", supervisorPort)
	}
}

func TestDaemonRemovesStaleSocket(t *testing.T) {
	socket := testSocketPath(t)

	// Leave a genuine socket file behind with nothing accepting on it.
	addr, err := net.ResolveUnixAddr("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("stale socket file should exist: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d := New(testConfig(freePort(t), freePort(t)), socket, t.TempDir(), log.New(io.Discard, "", 0))
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	defer func() {
		cancel()
		<-runDone
	}()

	waitForDaemon(t, socket, runDone)
}

func TestDaemonRefusesNonSocketFile(t *testing.T) {
	socket := testSocketPath(t)
	if err := os.WriteFile(socket, []byte("not a socket"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := New(testConfig(freePort(t), freePort(t)), socket, t.TempDir(), log.New(io.Discard, "", 0)).Run(context.Background())

	if err == nil {
		t.Fatal("daemon should refuse to replace a non-socket file")
	}
	if !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("error = %v, want it to explain the path is not a socket", err)
	}
}

func TestDaemonReportsSupervisorPortConflict(t *testing.T) {
	supervisorPort := freePort(t)
	blocker, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", supervisorPort))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close() //nolint:errcheck // test cleanup

	socket := testSocketPath(t)
	runErr := New(testConfig(supervisorPort, freePort(t)), socket, t.TempDir(), log.New(io.Discard, "", 0)).Run(context.Background())

	if runErr == nil {
		t.Fatal("daemon should fail when a supervisor port is taken")
	}
	if !strings.Contains(runErr.Error(), `"dashboard"`) {
		t.Errorf("error = %v, want it to name the project", runErr)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Errorf("control socket file should be cleaned up after failed start (stat err: %v)", err)
	}
}
