package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// wildcardFixture is a config directory holding one wildcard discovery
// entry over a workspace of fake worktrees, plus the daemon started from
// it. Each worktree carries a `port` file (read by both port_command and
// the dev-server command) and appends a line to `port-runs` every time
// port_command runs.
type wildcardFixture struct {
	t          *testing.T
	dir        string
	configPath string
	workspace  string
	repo       string
	command    string
	port       int // the entry's supervisor_port
	base       string
	socket     string
	client     *control.Client
}

func newWildcardFixture(t *testing.T) *wildcardFixture {
	t.Helper()
	command, err := testproc.Command()
	if err != nil {
		t.Fatal(err)
	}
	f := &wildcardFixture{
		t:         t,
		dir:       t.TempDir(),
		workspace: t.TempDir(),
		repo:      t.TempDir(),
		command:   command,
		port:      freePort(t),
		base:      "webapp.test",
	}
	f.configPath = filepath.Join(f.dir, "config.yaml")
	if err := os.MkdirAll(filepath.Join(f.repo, ".git", "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// worktree creates a linked worktree of the fixture repository with a
// `port` file holding a free application port and the given extra files.
func (f *wildcardFixture) worktree(name string, files ...string) (dir string, appPort int) {
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
	f.setPort(name, itoa(appPort))
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("x"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	return dir, appPort
}

func itoa(n int) string { return fmt.Sprint(n) }

// setPort rewrites a worktree's port file.
func (f *wildcardFixture) setPort(name, port string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.workspace, name, "port"), []byte(port+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// portRuns counts how often port_command ran in a worktree.
func (f *wildcardFixture) portRuns(name string) int {
	data, err := os.ReadFile(filepath.Join(f.workspace, name, "port-runs"))
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

// writeConfig writes the main config: a wildcard entry (with extra
// template lines) and the given projects body.
func (f *wildcardFixture) writeConfig(extra string, projects string) {
	f.t.Helper()
	body := fmt.Sprintf(`projects:
%sdiscovery:
  - name: webapp
    mode: wildcard
    base_domain: %s
    supervisor_port: %d
    directory: %s
    repository: %s
    require_files: [port]
    exclude: [".*", "tmp-*"]
    port_command: "echo run >> port-runs; cat port"
    command: "HW_TESTPROC_PORT=$(cat port) %s"
    readiness_strategy: tcp
    startup_timeout_seconds: 10
    idle_timeout_seconds: 60
    shutdown_timeout_seconds: 5
    hold_max_wait_seconds: 20
    env:
      %s: %s
%s`, projects, f.base, f.port, f.workspace, f.repo, f.command, testproc.EnvMode, testproc.ModeHTTP, extra)
	if err := os.WriteFile(f.configPath, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// writeRaw replaces the main config wholesale.
func (f *wildcardFixture) writeRaw(body string) {
	f.t.Helper()
	if err := os.WriteFile(f.configPath, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *wildcardFixture) start() {
	f.t.Helper()
	cfg, err := config.Load(f.configPath)
	if err != nil {
		f.t.Fatalf("load fixture config: %v", err)
	}
	f.socket, _, _ = startDaemon(f.t, cfg)
	f.client = control.NewClient(f.socket)
}

// get performs GET through the wildcard listener with the given Host
// header, returning the status, body, and (for a 200 echo) the pid.
func (f *wildcardFixture) get(host, path string, header http.Header) (int, string, int) {
	f.t.Helper()
	code, body := getHost(f.t, f.port, host, path, header)
	pid := 0
	if code == http.StatusOK {
		pid = echoPid(f.t, body)
	}
	return code, body, pid
}

// getHost performs GET http://127.0.0.1:port/path with an explicit Host.
func getHost(t *testing.T, port int, host, path string, header http.Header) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, vs := range header {
		req.Header[k] = vs
	}
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET :%d%s (Host %s): %v", port, path, host, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func (f *wildcardFixture) host(label string) string { return label + "." + f.base }

func (f *wildcardFixture) reload() *control.ReloadResponse {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := f.client.Reload(ctx)
	if err != nil {
		f.t.Fatalf("Reload: %v", err)
	}
	return resp
}

// hasProject reports whether the daemon status lists name.
func (f *wildcardFixture) hasProject(name string) bool {
	f.t.Helper()
	status, err := f.client.Status(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	for _, p := range status.Projects {
		if p.Name == name {
			return true
		}
	}
	return false
}

// waitDropped polls until the daemon status no longer lists name.
func (f *wildcardFixture) waitDropped(name string, timeout time.Duration) {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for f.hasProject(name) {
		if time.Now().After(deadline) {
			f.t.Fatalf("project %q still in status after %s", name, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWildcardMaterialisesWorktreesOnDemand: a request whose Host is
// <label>.<base_domain> cold-starts <directory>/<label>; each label is its
// own project, listed as dynamic; hosts that name no servable worktree —
// the base domain itself, a missing directory, a standalone repository, a
// worktree missing a required file, an excluded name — get a 404
// diagnostic that says why.
func TestWildcardMaterialisesWorktreesOnDemand(t *testing.T) {
	f := newWildcardFixture(t)
	aDir, aPort := f.worktree("a")
	f.worktree("b")
	f.worktree("no-port")
	if err := os.Remove(filepath.Join(f.workspace, "no-port", "port")); err != nil {
		t.Fatal(err)
	}
	f.worktree("tmp-scratch")
	if err := os.MkdirAll(filepath.Join(f.workspace, "standalone", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.writeConfig("", "")
	f.start()

	code, body, aPid := f.get(f.host("a"), "/hello?x=1", nil)
	if code != http.StatusOK || !strings.Contains(body, "ok GET /hello host=a.webapp.test") {
		t.Fatalf("a cold start = %d; body:\n%s", code, body)
	}
	code, body, bPid := f.get(f.host("b"), "/", nil)
	if code != http.StatusOK || !strings.Contains(body, "host=b.webapp.test") {
		t.Fatalf("b cold start = %d; body:\n%s", code, body)
	}
	if aPid == bPid {
		t.Errorf("a and b served by the same process %d", aPid)
	}
	// Case and port in the Host header do not matter.
	if _, _, pid := f.get("A.WebApp.TEST:443", "/again", nil); pid != aPid {
		t.Errorf("A.WebApp.TEST:443 served by pid %d, want a's %d", pid, aPid)
	}

	status, err := f.client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Projects) != 2 {
		t.Fatalf("status lists %d projects, want a and b: %+v", len(status.Projects), status.Projects)
	}
	pa := status.Projects[0]
	if pa.Name != "a" || !pa.Dynamic || pa.Source != "discovery:webapp" || pa.Host != "a.webapp.test" ||
		pa.PublicURL != "https://a.webapp.test" || pa.SupervisorPort != f.port || pa.ApplicationPort != aPort ||
		pa.WorkingDirectory != aDir || pa.State != StateRunning || pa.PID <= 0 {
		t.Errorf("a status = %+v", pa)
	}
	if len(status.Wildcards) != 1 || status.Wildcards[0].Name != "webapp" || status.Wildcards[0].Projects != 2 ||
		status.Wildcards[0].URLPattern != "https://<label>.webapp.test" || status.Wildcards[0].SupervisorPort != f.port {
		t.Errorf("wildcard status = %+v", status.Wildcards)
	}
	if f.portRuns("a") != 1 {
		t.Errorf("port_command ran %d times for a, want once", f.portRuns("a"))
	}

	for host, want := range map[string]string{
		"nope.webapp.test":        `no worktree named "nope"`,
		"webapp.test":             `no project for host "webapp.test"`,
		"www.a.webapp.test":       `no project for host "www.a.webapp.test"`,
		"other.test":              `no project for host "other.test"`,
		"standalone.webapp.test":  "standalone git repository",
		"no-port.webapp.test":     "missing required file(s): port",
		"tmp-scratch.webapp.test": "excluded",
	} {
		code, body, _ := f.get(host, "/", nil)
		if code != http.StatusNotFound {
			t.Errorf("Host %s = %d, want 404; body:\n%s", host, code, body)
		}
		if !strings.Contains(body, want) {
			t.Errorf("Host %s 404 body missing %q:\n%s", host, want, body)
		}
	}
	// Browsers get the HTML page.
	code, body, _ = f.get("nope.webapp.test", "/", http.Header{"Accept": []string{"text/html"}})
	if code != http.StatusNotFound || !strings.Contains(body, "<title>herd-wake: no worktree named") {
		t.Errorf("HTML 404 = %d; body:\n%s", code, body)
	}
	// Nothing above created a project.
	if n := len(f.statusProjects()); n != 2 {
		t.Errorf("status lists %d projects after the 404s, want 2", n)
	}
}

func (f *wildcardFixture) statusProjects() []control.ProjectStatus {
	f.t.Helper()
	status, err := f.client.Status(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return status.Projects
}

// TestWildcardConcurrentFirstRequestsShareOneResolution: many simultaneous
// first requests for one label run port_command once and start one
// process; every request is answered by it.
func TestWildcardConcurrentFirstRequestsShareOneResolution(t *testing.T) {
	f := newWildcardFixture(t)
	f.worktree("a")
	f.writeConfig(fmt.Sprintf("      %s: 300ms\n", testproc.EnvStartDelay), "")
	f.start()

	const requests = 20
	type result struct {
		code int
		body string
	}
	results := make([]result, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := getHost(t, f.port, f.host("a"), fmt.Sprintf("/n/%d", i), nil)
			results[i] = result{code, body}
		}()
	}
	wg.Wait()

	pids := map[int]bool{}
	for i, res := range results {
		if res.code != http.StatusOK {
			t.Errorf("request %d: %d; body: %s", i, res.code, res.body)
			continue
		}
		if !strings.Contains(res.body, fmt.Sprintf("ok GET /n/%d ", i)) {
			t.Errorf("request %d got someone else's response: %q", i, res.body)
		}
		pids[echoPid(t, res.body)] = true
	}
	if len(pids) != 1 {
		t.Errorf("answered by %d distinct processes, want 1", len(pids))
	}
	if runs := f.portRuns("a"); runs != 1 {
		t.Errorf("port_command ran %d times, want exactly once", runs)
	}
	if n := countSpawnsFor(t, f.socket, "a", "testproc serving http"); n != 1 {
		t.Errorf("%d processes spawned for a, want 1", n)
	}
}

// countSpawnsFor counts a project's startup markers in its logs.
func countSpawnsFor(t *testing.T, socket, name, marker string) int {
	t.Helper()
	logs, err := control.NewClient(socket).Logs(context.Background(), name, 0)
	if err != nil {
		t.Fatalf("Logs(%s): %v", name, err)
	}
	n := 0
	for _, line := range logs.Lines {
		if strings.Contains(line, marker) {
			n++
		}
	}
	return n
}

// TestWildcardVanishedDirectory: once a worktree directory is gone, the
// next request for its label is a 404; a running dynamic project is
// stopped and dropped, a stopped one is dropped at once, and one that
// idles out after its directory vanished is dropped by the idle stop. A
// re-created worktree materialises fresh.
func TestWildcardVanishedDirectory(t *testing.T) {
	f := newWildcardFixture(t)
	aDir, _ := f.worktree("a")
	bDir, _ := f.worktree("b")
	cDir, _ := f.worktree("c")
	f.writeConfig("", "")
	f.start()

	// a: running when its directory vanishes.
	_, _, aPid := f.get(f.host("a"), "/", nil)
	if err := os.RemoveAll(aDir); err != nil {
		t.Fatal(err)
	}
	code, body, _ := f.get(f.host("a"), "/", nil)
	if code != http.StatusNotFound || !strings.Contains(body, `no worktree named "a"`) {
		t.Errorf("a after removal = %d; body:\n%s", code, body)
	}
	f.waitDropped("a", 15*time.Second)
	waitProcessGone(t, aPid)

	// b: stopped by hand, then its directory vanishes.
	_, _, bPid := f.get(f.host("b"), "/", nil)
	if _, err := f.client.StopProject(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	waitProcessGone(t, bPid)
	if !f.hasProject("b") {
		t.Fatal("b should stay registered while its directory exists")
	}
	if err := os.RemoveAll(bDir); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := f.get(f.host("b"), "/", nil); code != http.StatusNotFound {
		t.Errorf("b after removal = %d, want 404", code)
	}
	f.waitDropped("b", 15*time.Second)

	// c: its directory vanishes while running; a manual stop drops it.
	_, _, cPid := f.get(f.host("c"), "/", nil)
	if err := os.RemoveAll(cDir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.StopProject(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	waitProcessGone(t, cPid)
	f.waitDropped("c", 15*time.Second)

	// Re-created worktree: served again, by a fresh process.
	f.worktree("a")
	code, body, pid := f.get(f.host("a"), "/", nil)
	if code != http.StatusOK || pid == aPid {
		t.Errorf("re-created a = %d pid %d (old %d); body:\n%s", code, pid, aPid, body)
	}
}

// TestWildcardIdleStopDropsVanished: the idle monitor's stop drops a
// dynamic project whose directory vanished while it ran.
func TestWildcardIdleStopDropsVanished(t *testing.T) {
	f := newWildcardFixture(t)
	aDir, _ := f.worktree("a")
	f.writeConfig("", "")
	data, _ := os.ReadFile(f.configPath)
	f.writeRaw(strings.ReplaceAll(string(data), "idle_timeout_seconds: 60", "idle_timeout_seconds: 1"))
	f.start()

	_, _, aPid := f.get(f.host("a"), "/", nil)
	if err := os.RemoveAll(aDir); err != nil {
		t.Fatal(err)
	}
	waitProcessGone(t, aPid)
	f.waitDropped("a", 15*time.Second)
}

// TestWildcardControlAPIByLabel: a dynamic project is an ordinary project
// for the control API — logs, lease, stop, restart by label — and
// project:restart re-runs port_command, moving the project to a new port
// when it changed.
func TestWildcardControlAPIByLabel(t *testing.T) {
	f := newWildcardFixture(t)
	f.worktree("a")
	f.writeConfig("", "")
	f.start()
	ctx := context.Background()

	_, _, pid1 := f.get(f.host("a"), "/", nil)
	logs, err := f.client.Logs(ctx, "a", 0)
	if err != nil || !strings.Contains(strings.Join(logs.Lines, "\n"), "testproc serving http") {
		t.Fatalf("Logs(a) = %+v, %v", logs, err)
	}
	if _, err := f.client.LeaseProject(ctx, "a", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if st := projectStatusByName(t, f.socket, "a"); st.LeaseUntil.IsZero() {
		t.Error("lease not recorded")
	}

	// Restart with an unchanged port: a fresh process, same port.
	before := projectStatusByName(t, f.socket, "a")
	restarted, err := f.client.RestartProject(ctx, "a")
	if err != nil {
		t.Fatalf("RestartProject(a): %v", err)
	}
	if restarted.PID == before.PID || restarted.ApplicationPort != before.ApplicationPort || restarted.State != StateRunning {
		t.Errorf("restart = %+v (old pid %d)", restarted, before.PID)
	}
	if runs := f.portRuns("a"); runs != 2 {
		t.Errorf("port_command ran %d times after one restart, want 2", runs)
	}
	waitProcessGone(t, pid1)

	// Restart after the port changed: the project moves to the new port,
	// keeps its lease, and serves.
	newPort := freePort(t)
	f.setPort("a", itoa(newPort))
	restarted, err = f.client.RestartProject(ctx, "a")
	if err != nil {
		t.Fatalf("RestartProject(a) after port change: %v", err)
	}
	if restarted.ApplicationPort != newPort || restarted.State != StateRunning || restarted.LeaseUntil.IsZero() {
		t.Errorf("restart after port change = %+v, want running on %d with the lease kept", restarted, newPort)
	}
	// The echoed pid is the helper's own; the status PID is the shell that spawned it
	// (equal only on exec-ing shells), so assert a fresh process rather than equality.
	code, body, pid3 := f.get(f.host("a"), "/moved", nil)
	if code != http.StatusOK || pid3 <= 0 || pid3 == pid1 {
		t.Errorf("after port change = %d pid %d (old pid %d); body:\n%s", code, pid3, pid1, body)
	}

	// Stop by label; the project stays registered (its directory exists).
	if _, err := f.client.StopProject(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if st := projectStatusByName(t, f.socket, "a"); st.State != StateStopped {
		t.Errorf("after stop = %q", st.State)
	}
	if _, _, pid := f.get(f.host("a"), "/", nil); pid == pid3 || pid == 0 {
		t.Errorf("after stop served by pid %d, want a fresh process", pid)
	}
}

// TestWildcardResolutionFailures: a failing port_command, or a port
// another live project uses, yields a 503 diagnostic with the reason;
// requests during the backoff do not re-run port_command; nothing is
// materialised.
func TestWildcardResolutionFailures(t *testing.T) {
	f := newWildcardFixture(t)
	_, aPort := f.worktree("a")
	f.worktree("broken")
	f.setPort("broken", "not-a-port")
	f.worktree("dup")
	f.setPort("dup", itoa(aPort))
	f.writeConfig("", "")
	f.start()

	if code, _, _ := f.get(f.host("a"), "/", nil); code != http.StatusOK {
		t.Fatalf("a = %d", code)
	}
	code, body, _ := f.get(f.host("broken"), "/", nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "port_command") || !strings.Contains(body, "not-a-port") {
		t.Errorf("broken = %d; body:\n%s", code, body)
	}
	code, body, _ = f.get(f.host("broken"), "/", nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "Automatic retry") {
		t.Errorf("broken during backoff = %d; body:\n%s", code, body)
	}
	if runs := f.portRuns("broken"); runs != 1 {
		t.Errorf("port_command ran %d times for broken, want once (backoff)", runs)
	}
	code, body, _ = f.get(f.host("dup"), "/", nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, fmt.Sprintf("%d", aPort)) || !strings.Contains(body, `project "a"`) {
		t.Errorf("dup = %d; body:\n%s", code, body)
	}
	if names := f.statusProjects(); len(names) != 1 || names[0].Name != "a" {
		t.Errorf("status = %+v, want only a", names)
	}
}

// TestWildcardStaticNameTakesPrecedence: a static project with the same
// name as a worktree label is served as configured, and the wildcard
// listener refuses the label with a 503 naming the conflict.
func TestWildcardStaticNameTakesPrecedence(t *testing.T) {
	f := newWildcardFixture(t)
	f.worktree("a")
	staticPorts := [2]int{freePort(t), freePort(t)}
	f.writeConfig("", fmt.Sprintf(`  a:
    public_url: https://a.test
    supervisor_port: %d
    application_port: %d
    working_directory: %s
    command: sleep 300
    readiness_strategy: tcp
`, staticPorts[0], staticPorts[1], f.dir))
	f.start()

	code, body, _ := f.get(f.host("a"), "/", nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `A project named "a" is already registered`) {
		t.Errorf("label a with a static a = %d; body:\n%s", code, body)
	}
}

// TestReloadWildcardEntries: reloading with the entry unchanged leaves its
// running dynamic projects untouched; a changed entry stops and drops them
// (they are listed as removed) and rebuilds the listener; a removed entry
// closes the listener.
func TestReloadWildcardEntries(t *testing.T) {
	f := newWildcardFixture(t)
	f.worktree("a")
	f.writeConfig("", "")
	f.start()

	_, _, pid1 := f.get(f.host("a"), "/", nil)
	before := projectStatusByName(t, f.socket, "a")

	// Unchanged.
	resp := f.reload()
	assertDiff(t, resp, "", "", "", "")
	if resp.Wildcards == nil || names(resp.Wildcards.Unchanged) != "webapp" {
		t.Fatalf("Wildcards = %+v, want webapp unchanged", resp.Wildcards)
	}
	after := projectStatusByName(t, f.socket, "a")
	// Status PIDs are the supervisor's direct child (the shell), which only equals the
	// echoed helper pid on shells that exec; compare status to status.
	if after.PID != before.PID || after.State != StateRunning || !after.IdleStopAt.Equal(before.IdleStopAt) {
		t.Errorf("a after unchanged reload = %+v, want untouched (pid %d)", after, before.PID)
	}
	if _, _, pid := f.get(f.host("a"), "/", nil); pid != pid1 {
		t.Errorf("a served by %d after unchanged reload, want %d", pid, pid1)
	}

	// Changed: the dynamic project is stopped and dropped; the listener is
	// rebuilt and the label materialises again on demand.
	f.writeConfig("    rewrite_host: true\n", "")
	resp = f.reload()
	assertDiff(t, resp, "", "a", "", "")
	if names(resp.Wildcards.Changed) != "webapp" || len(resp.Errors) != 0 {
		t.Fatalf("Wildcards = %+v, errors %v", resp.Wildcards, resp.Errors)
	}
	waitProcessGone(t, pid1)
	if f.hasProject("a") {
		t.Error("a still registered after its entry changed")
	}
	code, body, pid2 := f.get(f.host("a"), "/", nil)
	if code != http.StatusOK || pid2 == pid1 {
		t.Fatalf("a after entry change = %d pid %d; body:\n%s", code, pid2, body)
	}
	if !strings.Contains(body, "host=127.0.0.1:") {
		t.Errorf("rewrite_host from the changed template not applied; body: %s", body)
	}

	// Removed: dynamic projects go, the port closes.
	f.writeRaw("projects: {}\n")
	resp = f.reload()
	assertDiff(t, resp, "", "a", "", "")
	if names(resp.Wildcards.Removed) != "webapp" {
		t.Fatalf("Wildcards = %+v, want webapp removed", resp.Wildcards)
	}
	waitProcessGone(t, pid2)
	if portAccepts(f.port) {
		t.Errorf("wildcard port %d still accepts connections after the entry was removed", f.port)
	}
	status, err := f.client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Projects) != 0 || len(status.Wildcards) != 0 {
		t.Errorf("status after removal = %+v / %+v, want empty", status.Projects, status.Wildcards)
	}

	// Added back: served again.
	f.writeConfig("", "")
	resp = f.reload()
	if names(resp.Wildcards.Added) != "webapp" {
		t.Fatalf("Wildcards = %+v, want webapp added", resp.Wildcards)
	}
	if code, _, _ := f.get(f.host("a"), "/", nil); code != http.StatusOK {
		t.Errorf("a after the entry was re-added = %d", code)
	}
}

// TestWildcardEntryBindFailureAbortsStart: a taken wildcard port is a
// startup error naming the entry, like a taken project port.
func TestWildcardEntryBindFailureAbortsStart(t *testing.T) {
	f := newWildcardFixture(t)
	f.writeConfig("", "")
	blocker, err := netListen(f.port)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close() //nolint:errcheck // test cleanup
	cfg, err := config.Load(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	socket := testSocketPath(t)
	runErr := New(cfg, socket, t.TempDir(), discardLogger()).Run(context.Background())
	if runErr == nil || !strings.Contains(runErr.Error(), `discovery "webapp"`) || !strings.Contains(runErr.Error(), "listen on") {
		t.Errorf("Run with the wildcard port taken = %v, want a bind error naming the entry", runErr)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Errorf("control socket should be cleaned up after the failed start (stat err: %v)", err)
	}
}
