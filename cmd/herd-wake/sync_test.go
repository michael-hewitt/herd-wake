package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

func TestMain(m *testing.M) {
	testproc.Main() // exits the process when running as a helper
	os.Exit(m.Run())
}

// syncFixture is a config directory with one discovery entry over a
// workspace of fake worktrees, plus a fake herd wired into HOME and PATH so
// the CLI resolves it instead of any real Herd on this machine.
type syncFixture struct {
	t          *testing.T
	configDir  string
	configPath string
	workspace  string
	repo       string
	home       string
	herdLog    string
	nginxDir   string
	socket     string
	// supervisorLow is the start of the supervisor port range (a free
	// port, so a daemon can bind the allocated ports).
	supervisorLow int
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	f := &syncFixture{
		t:             t,
		configDir:     t.TempDir(),
		workspace:     t.TempDir(),
		repo:          t.TempDir(),
		home:          t.TempDir(),
		socket:        testSocketPath(t),
		supervisorLow: freePort(t),
	}
	f.configPath = filepath.Join(f.configDir, "config.yaml")
	if err := os.MkdirAll(filepath.Join(f.repo, ".git", "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.herdLog = filepath.Join(f.home, "herd.log")
	f.nginxDir = filepath.Join(f.home, "Library", "Application Support", "Herd", "config", "valet", "Nginx")
	binDir := filepath.Join(f.home, "fakebin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := testproc.WriteFakeHerd(binDir, testproc.FakeHerd{LogFile: f.herdLog, NginxDir: f.nginxDir}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", f.home)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

// worktree creates a fake linked worktree holding a `port` file with a
// free application port (read by the template's port_command).
func (f *syncFixture) worktree(name string) (dir string, appPort int) {
	f.t.Helper()
	dir = filepath.Join(f.workspace, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	admin := filepath.Join(f.repo, ".git", "worktrees", name)
	if err := os.MkdirAll(admin, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	appPort = freePort(f.t)
	if err := os.WriteFile(filepath.Join(dir, "port"), []byte(fmt.Sprintf("%d\n", appPort)), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return dir, appPort
}

// writeConfig writes the main config with the discovery entry; the
// template command re-invokes the test binary as an HTTP server on the
// port from the worktree's port file.
func (f *syncFixture) writeConfig(projects string) {
	f.t.Helper()
	command, err := testproc.Command()
	if err != nil {
		f.t.Fatal(err)
	}
	body := fmt.Sprintf(`projects:
%sdiscovery:
  - name: webapp
    directory: %s
    repository: %s
    supervisor_port_range: [%d, %d]
    port_command: cat port
    command: HW_TESTPROC_PORT=$(cat port) %s
    readiness_strategy: tcp
    startup_timeout_seconds: 20
    shutdown_timeout_seconds: 5
    env:
      %s: %s
`, projects, f.workspace, f.repo, f.supervisorLow, f.supervisorLow+200, command, testproc.EnvMode, testproc.ModeHTTP)
	if err := os.WriteFile(f.configPath, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *syncFixture) run(args ...string) (code int, stdout, stderr string) {
	f.t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func (f *syncFixture) managedFile() string {
	return filepath.Join(f.configDir, "projects.d", "webapp.yaml")
}

// startDaemon runs `herd-wake start` in a goroutine against the fixture
// config and waits until it answers; it is stopped with SIGINT on cleanup.
func (f *syncFixture) startDaemon() {
	f.t.Helper()
	var startOut, startErr bytes.Buffer
	var mu sync.Mutex
	startDone := make(chan int, 1)
	go func() {
		mu.Lock()
		defer mu.Unlock()
		startDone <- run([]string{"start", "--config", f.configPath, "--socket", f.socket, "--log-dir", f.t.TempDir()}, &startOut, &startErr)
	}()
	f.t.Cleanup(func() {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			f.t.Errorf("send SIGINT: %v", err)
			return
		}
		select {
		case code := <-startDone:
			if code != 0 {
				mu.Lock()
				defer mu.Unlock()
				f.t.Errorf("start exit code = %d after SIGINT (stderr:\n%s)", code, startErr.String())
			}
		case <-time.After(20 * time.Second):
			f.t.Error("start did not exit after SIGINT")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if code, _, _ := f.run("status", "--socket", f.socket); code == 0 {
			return
		}
		select {
		case code := <-startDone:
			f.t.Fatalf("start exited early with code %d", code)
		default:
		}
		if time.Now().After(deadline) {
			f.t.Fatal("daemon never answered on the control socket")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// portAccepts reports whether something accepts TCP connections on port.
func portAccepts(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// TestRunSyncEndToEndWithDaemon: sync registers a discovered worktree,
// triggers a reload, and the new project's supervisor port serves through
// the daemon; project:remove takes it away again, unproxies, and reloads.
func TestRunSyncEndToEndWithDaemon(t *testing.T) {
	f := newSyncFixture(t)
	f.worktree("alpha")
	f.writeConfig("")
	f.startDaemon()

	code, stdout, stderr := f.run("sync", "--config", f.configPath, "--socket", f.socket)
	if code != 0 {
		t.Fatalf("sync exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		`Discovery "webapp"`,
		"added:     alpha (https://alpha.test, supervisor " + fmt.Sprint(f.supervisorLow),
		"herd:      proxy alpha → http://127.0.0.1:" + fmt.Sprint(f.supervisorLow) + " (done)",
		"file:      written",
		"Daemon: reloaded — added alpha, removed (none), changed (none), unchanged (none)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("sync output missing %q; got:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(f.managedFile()); err != nil {
		t.Fatalf("managed file not written: %v", err)
	}
	if got := testproc.ReadFakeHerdLog(f.herdLog); len(got) != 3 || got[2] != fmt.Sprintf("proxy alpha http://127.0.0.1:%d --secure", f.supervisorLow) {
		t.Errorf("herd calls = %v, want paths, links, and one proxy", got)
	}

	// The new project's supervisor port serves (cold start through the
	// daemon, via the worktree's command).
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/hello", f.supervisorLow))
	if err != nil {
		t.Fatalf("GET through the new supervisor port: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ok GET /hello") {
		t.Fatalf("GET = %d %q, want 200 from the worktree's server", resp.StatusCode, body)
	}

	// A second sync is a no-op that still reloads (everything unchanged).
	code, stdout, stderr = f.run("sync", "--config", f.configPath, "--socket", f.socket)
	if code != 0 {
		t.Fatalf("second sync exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, want := range []string{"unchanged: alpha", "file:      up to date", "skipped; already proxied", "unchanged alpha"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("second sync output missing %q; got:\n%s", want, stdout)
		}
	}
	if code, _, _ := f.run("status", "--socket", f.socket); code != 0 {
		t.Fatal("daemon stopped answering")
	}

	// project:remove: gone from the file, unproxied, reloaded, port closed.
	code, stdout, stderr = f.run("project:remove", "--config", f.configPath, "--socket", f.socket, "alpha")
	if code != 0 {
		t.Fatalf("project:remove exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		`Removed project "alpha" from ` + f.managedFile(),
		"herd: unproxy alpha (done)",
		"Daemon: reloaded — added (none), removed alpha",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("project:remove output missing %q; got:\n%s", want, stdout)
		}
	}
	calls := testproc.ReadFakeHerdLog(f.herdLog)
	if calls[len(calls)-1] != "unproxy alpha" {
		t.Errorf("herd calls = %v, want unproxy alpha last", calls)
	}
	deadline := time.Now().Add(5 * time.Second)
	for portAccepts(f.supervisorLow) {
		if time.Now().After(deadline) {
			t.Fatal("supervisor port still accepts connections after project:remove")
		}
		time.Sleep(50 * time.Millisecond)
	}
	code, _, stderr = f.run("project:remove", "--config", f.configPath, "--socket", f.socket, "alpha")
	if code != 0 || !strings.Contains(stderr, "nothing to remove") {
		t.Errorf("second project:remove: code=%d stderr=%q, want a no-op", code, stderr)
	}
}

func TestRunSyncDryRunJSON(t *testing.T) {
	f := newSyncFixture(t)
	f.worktree("alpha")
	f.writeConfig("")

	code, stdout, stderr := f.run("sync", "--config", f.configPath, "--socket", f.socket, "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("sync exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	var out struct {
		DryRun        bool `json:"dry_run"`
		HerdAvailable bool `json:"herd_available"`
		Discovery     []struct {
			Name    string `json:"name"`
			Changed bool   `json:"changed"`
			Written bool   `json:"written"`
			Added   []struct {
				Name            string `json:"name"`
				SupervisorPort  int    `json:"supervisor_port"`
				ApplicationPort int    `json:"application_port"`
			} `json:"added"`
			Herd []struct {
				Action  string `json:"action"`
				Status  string `json:"status"`
				Command string `json:"command"`
			} `json:"herd"`
		} `json:"discovery"`
		Reload struct {
			Attempted bool `json:"attempted"`
		} `json:"reload"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("sync --json output is not JSON: %v\n%s", err, stdout)
	}
	if !out.DryRun || !out.HerdAvailable || out.Reload.Attempted {
		t.Errorf("top level = %+v", out)
	}
	if len(out.Discovery) != 1 || out.Discovery[0].Name != "webapp" || !out.Discovery[0].Changed || out.Discovery[0].Written {
		t.Fatalf("discovery = %+v", out.Discovery)
	}
	e := out.Discovery[0]
	if len(e.Added) != 1 || e.Added[0].Name != "alpha" || e.Added[0].SupervisorPort != f.supervisorLow || e.Added[0].ApplicationPort == 0 {
		t.Errorf("added = %+v", e.Added)
	}
	if len(e.Herd) != 1 || e.Herd[0].Action != "proxy" || e.Herd[0].Status != "dry-run" || !strings.HasPrefix(e.Herd[0].Command, "herd proxy alpha ") {
		t.Errorf("herd = %+v", e.Herd)
	}
	if _, err := os.Stat(f.managedFile()); !os.IsNotExist(err) {
		t.Error("dry run wrote the managed file")
	}
	for _, call := range testproc.ReadFakeHerdLog(f.herdLog) {
		if call != "paths" && call != "links" {
			t.Errorf("dry run ran herd %q", call)
		}
	}
}

func TestRunSyncDaemonNotRunningAndNoHerd(t *testing.T) {
	f := newSyncFixture(t)
	f.worktree("alpha")
	f.writeConfig("")

	code, stdout, stderr := f.run("sync", "--config", f.configPath, "--socket", f.socket, "--no-herd")
	if code != 0 {
		t.Fatalf("sync exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"added:     alpha",
		"Run these Herd commands yourself:",
		fmt.Sprintf("  herd proxy alpha http://127.0.0.1:%d --secure", f.supervisorLow),
		"Daemon: not running",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("sync output missing %q; got:\n%s", want, stdout)
		}
	}
	if calls := testproc.ReadFakeHerdLog(f.herdLog); len(calls) != 0 {
		t.Errorf("--no-herd ran herd: %v", calls)
	}
	if _, err := os.Stat(f.managedFile()); err != nil {
		t.Error("managed file should be written even without Herd and daemon")
	}
}

func TestRunSyncWithoutDiscoveryEntries(t *testing.T) {
	f := newSyncFixture(t)
	if err := os.WriteFile(f.configPath, []byte("projects:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := f.run("sync", "--config", f.configPath, "--socket", f.socket)
	if code != 1 || !strings.Contains(stderr, "no discovery entries") {
		t.Errorf("sync without discovery: code=%d stderr=%q", code, stderr)
	}
}

func TestRunSyncRejectsPositionalArgs(t *testing.T) {
	f := newSyncFixture(t)
	code, _, stderr := f.run("sync", "--config", f.configPath, "extra")
	if code != 2 || !strings.Contains(stderr, "usage") {
		t.Errorf("sync extra: code=%d stderr=%q", code, stderr)
	}
}

func TestRunProjectRemoveMainConfigProject(t *testing.T) {
	f := newSyncFixture(t)
	f.writeConfig(`  dashboard:
    public_url: https://vite.dashboard.test
    supervisor_port: 7101
    application_port: 17101
    working_directory: /tmp
    command: npm run dev
`)
	code, _, stderr := f.run("project:remove", "--config", f.configPath, "--socket", f.socket, "dashboard")
	if code != 1 {
		t.Fatalf("project:remove of a main-config project: code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"never rewrites", "remove it there by hand", "herd unproxy vite.dashboard", "herd-wake reload"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q; got:\n%s", want, stderr)
		}
	}
	if calls := testproc.ReadFakeHerdLog(f.herdLog); len(calls) != 0 {
		t.Errorf("herd was called: %v", calls)
	}
}

func TestRunProjectRemoveRequiresName(t *testing.T) {
	f := newSyncFixture(t)
	code, _, stderr := f.run("project:remove", "--config", f.configPath)
	if code != 2 || !strings.Contains(stderr, "usage") {
		t.Errorf("project:remove without a name: code=%d stderr=%q", code, stderr)
	}
}

func TestUsageMentionsSyncAndRemove(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run(nil, &stdout, &stderr)
	for _, want := range []string{"sync", "project:remove", "--dry-run", "--keep-herd"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("usage should mention %q", want)
		}
	}
}
