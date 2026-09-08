package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// reloadFixture is a config directory (main file + projects.d) the reload
// tests rewrite between reloads, plus the daemon started from it.
type reloadFixture struct {
	t          *testing.T
	dir        string
	configPath string
	command    string
	socket     string
	client     *control.Client
	// ports are allocated once per project name so rewriting a project
	// without touching its ports leaves it unchanged.
	ports map[string][2]int
}

func newReloadFixture(t *testing.T) *reloadFixture {
	t.Helper()
	command, err := testproc.Command()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return &reloadFixture{
		t:          t,
		dir:        dir,
		configPath: filepath.Join(dir, "config.yaml"),
		command:    command,
		ports:      map[string][2]int{},
	}
}

// projectSpec describes one project block for the fixture's config files.
type projectSpec struct {
	name string
	// mode is the testproc mode the command runs in (default ModeHTTP).
	mode string
	// commandPrefix is prepended to the helper command (e.g. an echo that
	// marks the log), so the command — and only the command — changes.
	commandPrefix string
	// supervisorPort overrides the port allocated for the name.
	supervisorPort int
	alwaysOn       bool
	shutdownSecs   int
}

// yaml renders the project block, allocating ports on first use of the
// name.
func (f *reloadFixture) yaml(spec projectSpec) string {
	f.t.Helper()
	ports, ok := f.ports[spec.name]
	if !ok {
		ports = [2]int{freePort(f.t), freePort(f.t)}
		f.ports[spec.name] = ports
	}
	supervisorPort := ports[0]
	if spec.supervisorPort != 0 {
		supervisorPort = spec.supervisorPort
	}
	mode := spec.mode
	if mode == "" {
		mode = testproc.ModeHTTP
	}
	shutdown := spec.shutdownSecs
	if shutdown == 0 {
		shutdown = 5
	}
	command := f.command
	if spec.commandPrefix != "" {
		command = spec.commandPrefix + "; " + command
	}
	return fmt.Sprintf(`  %s:
    public_url: https://%s.test
    supervisor_port: %d
    application_port: %d
    working_directory: %s
    command: "%s"
    readiness_strategy: tcp
    startup_timeout_seconds: 10
    idle_timeout_seconds: 60
    shutdown_timeout_seconds: %d
    hold_max_wait_seconds: 20
    always_on: %v
    env:
      %s: %s
      %s: "%d"
`, spec.name, spec.name, supervisorPort, ports[1], f.dir, command, shutdown, spec.alwaysOn,
		testproc.EnvMode, mode, testproc.EnvPort, ports[1])
}

// supervisorPort returns the supervisor port allocated for name.
func (f *reloadFixture) supervisorPort(name string) int {
	return f.ports[name][0]
}

func (f *reloadFixture) writeMain(blocks ...string) {
	f.t.Helper()
	if err := os.WriteFile(f.configPath, []byte("projects:\n"+strings.Join(blocks, "")), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *reloadFixture) writeRaw(body string) {
	f.t.Helper()
	if err := os.WriteFile(f.configPath, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *reloadFixture) writeFragment(file string, blocks ...string) {
	f.t.Helper()
	dir := config.ProjectsDir(f.configPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte("projects:\n"+strings.Join(blocks, "")), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *reloadFixture) removeFragment(file string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(config.ProjectsDir(f.configPath), file)); err != nil {
		f.t.Fatal(err)
	}
}

// start loads the config from disk (so the daemon knows its path) and runs
// the daemon.
func (f *reloadFixture) start() {
	f.t.Helper()
	cfg, err := config.Load(f.configPath)
	if err != nil {
		f.t.Fatalf("load fixture config: %v", err)
	}
	f.socket, _, _ = startDaemon(f.t, cfg)
	f.client = control.NewClient(f.socket)
}

func (f *reloadFixture) reload() *control.ReloadResponse {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := f.client.Reload(ctx)
	if err != nil {
		f.t.Fatalf("Reload: %v", err)
	}
	return resp
}

// get performs one request through the named project's supervisor port and
// returns the status, body, and (for a 200 echo) the serving pid.
func (f *reloadFixture) get(name, path string) (int, string, int) {
	f.t.Helper()
	code, body := getPort(f.t, f.supervisorPort(name), path)
	pid := 0
	if code == http.StatusOK {
		pid = echoPid(f.t, body)
	}
	return code, body, pid
}

// getPort performs GET http://127.0.0.1:port/path.
func getPort(t *testing.T, port int, path string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		t.Fatalf("GET :%d%s: %v", port, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
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

func names(list []string) string { return strings.Join(list, ",") }

// assertDiff checks the reload's classification of every project.
func assertDiff(t *testing.T, resp *control.ReloadResponse, added, removed, changed, unchanged string) {
	t.Helper()
	if !resp.Applied {
		t.Fatalf("reload not applied; errors: %v", resp.Errors)
	}
	if got := names(resp.Added); got != added {
		t.Errorf("Added = %q, want %q", got, added)
	}
	if got := names(resp.Removed); got != removed {
		t.Errorf("Removed = %q, want %q", got, removed)
	}
	if got := names(resp.Changed); got != changed {
		t.Errorf("Changed = %q, want %q", got, changed)
	}
	if got := names(resp.Unchanged); got != unchanged {
		t.Errorf("Unchanged = %q, want %q", got, unchanged)
	}
}

// TestReloadAddsProjectLeavingOthersUntouched: adding project B (via a
// projects.d file) makes B's port serve, while running project A keeps its
// process, its idle deadline, and its lease.
func TestReloadAddsProjectLeavingOthersUntouched(t *testing.T) {
	f := newReloadFixture(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha"}))
	f.start()

	if _, err := f.client.LeaseProject(context.Background(), "alpha", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.ReleaseProjectLease(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	code, body, alphaPid := f.get("alpha", "/")
	if code != http.StatusOK {
		t.Fatalf("alpha cold start = %d; body:\n%s", code, body)
	}
	before := projectStatusByName(t, f.socket, "alpha")
	if before.State != StateRunning || before.IdleStopAt.IsZero() {
		t.Fatalf("alpha before reload = %+v, want running with a scheduled idle stop", before)
	}

	f.writeFragment("beta.yaml", f.yaml(projectSpec{name: "beta"}))
	resp := f.reload()

	assertDiff(t, resp, "beta", "", "", "alpha")
	if len(resp.Errors) != 0 || resp.ReloadedAt.IsZero() {
		t.Errorf("reload response = %+v, want no errors and a reload time", resp)
	}
	if want, _ := filepath.Abs(f.configPath); resp.ConfigPath != want {
		t.Errorf("ConfigPath = %q, want %q", resp.ConfigPath, want)
	}

	// B serves (cold start on its own port).
	code, body, _ = f.get("beta", "/")
	if code != http.StatusOK {
		t.Fatalf("beta after reload = %d; body:\n%s", code, body)
	}

	// A is untouched: same process, same idle deadline, still serving.
	after := projectStatusByName(t, f.socket, "alpha")
	if after.PID != before.PID || after.State != StateRunning {
		t.Errorf("alpha after reload = pid %d state %q, want pid %d running", after.PID, after.State, before.PID)
	}
	if !after.IdleStopAt.Equal(before.IdleStopAt) {
		t.Errorf("alpha IdleStopAt changed across reload: %v -> %v", before.IdleStopAt, after.IdleStopAt)
	}
	if _, _, pid := f.get("alpha", "/again"); pid != alphaPid {
		t.Errorf("alpha served by pid %d after reload, want %d", pid, alphaPid)
	}

	// Status reflects the reload.
	status, err := f.client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.LastReloadAt.IsZero() || status.ConfigPath != resp.ConfigPath {
		t.Errorf("status config fields = (%q, %v), want the config path and a reload time", status.ConfigPath, status.LastReloadAt)
	}
	if len(status.Projects) != 2 {
		t.Errorf("status lists %d projects, want 2", len(status.Projects))
	}
}

// TestReloadAddedAlwaysOnStartsImmediately: an always_on project added by a
// reload starts without any request.
func TestReloadAddedAlwaysOnStartsImmediately(t *testing.T) {
	f := newReloadFixture(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha"}))
	f.start()

	f.writeFragment("beta.yaml", f.yaml(projectSpec{name: "beta", alwaysOn: true}))
	resp := f.reload()

	assertDiff(t, resp, "beta", "", "", "alpha")
	waitForProjectState(t, f.socket, "beta", StateRunning, 10*time.Second)
}

// TestReloadRemovesProjectStoppingItAndClosingPort: removing a running
// project drains its process group and its supervisor port stops accepting
// connections; the other project is unaffected.
func TestReloadRemovesProjectStoppingItAndClosingPort(t *testing.T) {
	f := newReloadFixture(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha"}), f.yaml(projectSpec{name: "beta"}))
	f.start()

	_, _, alphaPid := f.get("alpha", "/")
	_, _, betaPid := f.get("beta", "/")
	betaSupervisorPid := projectStatusByName(t, f.socket, "beta").PID

	f.writeMain(f.yaml(projectSpec{name: "alpha"}))
	resp := f.reload()

	assertDiff(t, resp, "", "beta", "", "alpha")
	waitProcessGone(t, betaPid)
	waitProcessGone(t, betaSupervisorPid)
	if portAccepts(f.supervisorPort("beta")) {
		t.Errorf("beta's supervisor port %d still accepts connections after removal", f.supervisorPort("beta"))
	}
	status, err := f.client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Projects) != 1 || status.Projects[0].Name != "alpha" {
		t.Errorf("status projects = %+v, want only alpha", status.Projects)
	}
	var apiErr *control.APIError
	if _, err := f.client.StopProject(context.Background(), "beta"); !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
		t.Errorf("StopProject(beta) after removal = %v, want 404", err)
	}
	if _, _, pid := f.get("alpha", "/"); pid != alphaPid {
		t.Errorf("alpha served by pid %d after beta's removal, want %d", pid, alphaPid)
	}
}

// TestReloadChangedCommandStopsThenColdStartsNewCommand: changing a running
// project's command stops it (whole group drained); the next request
// cold-starts the new command on the same port. Its lease survives.
func TestReloadChangedCommandStopsThenColdStartsNewCommand(t *testing.T) {
	f := newReloadFixture(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha"}))
	f.start()

	_, _, pid1 := f.get("alpha", "/")
	supervisorPid := projectStatusByName(t, f.socket, "alpha").PID
	if _, err := f.client.LeaseProject(context.Background(), "alpha", 10*time.Minute); err != nil {
		t.Fatal(err)
	}

	f.writeMain(f.yaml(projectSpec{name: "alpha", commandPrefix: "echo reload-marker-v2"}))
	resp := f.reload()

	assertDiff(t, resp, "", "", "alpha", "")
	// Stopped by the time the reload returns; the old group is gone.
	if st := projectStatusByName(t, f.socket, "alpha"); st.State != StateStopped {
		t.Errorf("alpha right after reload = %q, want %q", st.State, StateStopped)
	}
	waitProcessGone(t, pid1)
	waitProcessGone(t, supervisorPid)
	if st := projectStatusByName(t, f.socket, "alpha"); st.LeaseUntil.IsZero() {
		t.Error("alpha's lease did not survive the config change")
	}

	// Next request: cold start with the new command.
	code, body, pid2 := f.get("alpha", "/")
	if code != http.StatusOK {
		t.Fatalf("alpha after change = %d; body:\n%s", code, body)
	}
	if pid2 == pid1 {
		t.Errorf("alpha still served by the old process %d", pid1)
	}
	logs, err := f.client.Logs(context.Background(), "alpha", 0)
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(logs.Lines, "\n"); !strings.Contains(joined, "reload-marker-v2") {
		t.Errorf("new command's marker missing from logs:\n%s", joined)
	}
}

// TestReloadChangedStoppedProjectJustSwapsConfig: a stopped project whose
// config changes is swapped without any process activity; it cold-starts
// the new command on demand.
func TestReloadChangedStoppedProjectJustSwapsConfig(t *testing.T) {
	f := newReloadFixture(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha"}))
	f.start()

	f.writeMain(f.yaml(projectSpec{name: "alpha", commandPrefix: "echo swapped-while-stopped"}))
	resp := f.reload()

	assertDiff(t, resp, "", "", "alpha", "")
	if code, _, _ := f.get("alpha", "/"); code != http.StatusOK {
		t.Fatalf("alpha after swap = %d", code)
	}
	logs, err := f.client.Logs(context.Background(), "alpha", 0)
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(logs.Lines, "\n"); !strings.Contains(joined, "swapped-while-stopped") {
		t.Errorf("new command's marker missing from logs:\n%s", joined)
	}
}

// TestReloadChangedSupervisorPortRebinds: changing supervisor_port releases
// the old port and serves on the new one.
func TestReloadChangedSupervisorPortRebinds(t *testing.T) {
	f := newReloadFixture(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha"}))
	f.start()
	oldPort := f.supervisorPort("alpha")
	_, _, pid1 := f.get("alpha", "/")

	newPort := freePort(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha", supervisorPort: newPort}))
	resp := f.reload()

	assertDiff(t, resp, "", "", "alpha", "")
	if portAccepts(oldPort) {
		t.Errorf("old supervisor port %d still accepts connections", oldPort)
	}
	code, body := getPort(t, newPort, "/")
	if code != http.StatusOK {
		t.Fatalf("new port %d = %d; body:\n%s", newPort, code, body)
	}
	if pid := echoPid(t, body); pid == pid1 {
		t.Errorf("served by the old process %d after a port change", pid1)
	}
	if st := projectStatusByName(t, f.socket, "alpha"); st.SupervisorPort != newPort {
		t.Errorf("status SupervisorPort = %d, want %d", st.SupervisorPort, newPort)
	}
}

// TestReloadRejectsInvalidConfig: an invalid config (validation errors,
// then malformed YAML) is rejected with the errors, nothing changes, and
// the daemon keeps serving; a corrected config reloads normally.
func TestReloadRejectsInvalidConfig(t *testing.T) {
	f := newReloadFixture(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha"}))
	f.start()
	_, _, pid1 := f.get("alpha", "/")

	f.writeRaw("projects:\n" + f.yaml(projectSpec{name: "alpha"}) + `  beta:
    public_url: "not a url"
    supervisor_port: 7101
    application_port: 17101
    working_directory: ` + f.dir + "\n")
	resp := f.reload()

	if resp.Applied {
		t.Fatalf("invalid config was applied: %+v", resp)
	}
	joined := strings.Join(resp.Errors, "\n")
	for _, want := range []string{`project "beta": command:`, `project "beta": public_url:`} {
		if !strings.Contains(joined, want) {
			t.Errorf("errors missing %q; got:\n%s", want, joined)
		}
	}
	if len(resp.Added)+len(resp.Removed)+len(resp.Changed)+len(resp.Unchanged) != 0 {
		t.Errorf("rejected reload classified projects: %+v", resp)
	}

	// Malformed YAML is rejected the same way.
	f.writeRaw("projects: [\n")
	if resp := f.reload(); resp.Applied || len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0], "parse config") {
		t.Errorf("malformed config reload = %+v, want rejected with a parse error", resp)
	}

	// Nothing changed and the daemon keeps serving.
	status, err := f.client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.LastReloadAt.IsZero() {
		t.Errorf("LastReloadAt = %v after rejected reloads, want zero", status.LastReloadAt)
	}
	if len(status.Projects) != 1 || status.Projects[0].PID != projectStatusByName(t, f.socket, "alpha").PID {
		t.Errorf("status changed after rejected reload: %+v", status.Projects)
	}
	if _, _, pid := f.get("alpha", "/"); pid != pid1 {
		t.Errorf("alpha served by pid %d after rejected reload, want %d", pid, pid1)
	}

	// Fixed config: alpha is unchanged and keeps its process.
	f.writeMain(f.yaml(projectSpec{name: "alpha"}))
	resp = f.reload()
	assertDiff(t, resp, "", "", "", "alpha")
	if _, _, pid := f.get("alpha", "/"); pid != pid1 {
		t.Errorf("alpha served by pid %d after unchanged reload, want %d", pid, pid1)
	}
}

// TestReloadBindFailureIsReportedWithoutAbortingRest: an added project
// whose port is taken is reported in Errors; the other added project is
// bound. Once the port frees up, the next reload adds it.
func TestReloadBindFailureIsReportedWithoutAbortingRest(t *testing.T) {
	f := newReloadFixture(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha"}))
	f.start()

	blocked := freePort(t)
	blocker, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", blocked))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close() //nolint:errcheck // test cleanup

	f.writeFragment("beta.yaml", f.yaml(projectSpec{name: "beta", supervisorPort: blocked}))
	f.writeFragment("gamma.yaml", f.yaml(projectSpec{name: "gamma"}))
	resp := f.reload()

	assertDiff(t, resp, "beta,gamma", "", "", "alpha")
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0], `project "beta"`) || !strings.Contains(resp.Errors[0], "listen on") {
		t.Errorf("Errors = %v, want one bind error naming beta", resp.Errors)
	}
	status, err := f.client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, p := range status.Projects {
		listed = append(listed, p.Name)
	}
	if names(listed) != "alpha,gamma" {
		t.Errorf("status projects = %v, want alpha and gamma (beta failed to bind)", listed)
	}
	if code, _, _ := f.get("gamma", "/"); code != http.StatusOK {
		t.Errorf("gamma = %d, want 200", code)
	}

	// Port freed: beta is added by the next reload.
	if err := blocker.Close(); err != nil {
		t.Fatal(err)
	}
	resp = f.reload()
	assertDiff(t, resp, "beta", "", "", "alpha,gamma")
	if len(resp.Errors) != 0 {
		t.Errorf("Errors = %v, want none", resp.Errors)
	}
	if code, _ := getPort(t, blocked, "/"); code != http.StatusOK {
		t.Errorf("beta on its freed port = %d, want 200", code)
	}
}

// TestReloadChangedPortBindFailureKeepsPreviousRegistration: when a changed
// project's new port cannot be bound, the error is reported and the project
// keeps serving on its previous port; the next reload retries.
func TestReloadChangedPortBindFailureKeepsPreviousRegistration(t *testing.T) {
	f := newReloadFixture(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha"}))
	f.start()
	oldPort := f.supervisorPort("alpha")
	f.get("alpha", "/")

	blocked := freePort(t)
	blocker, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", blocked))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close() //nolint:errcheck // test cleanup

	f.writeMain(f.yaml(projectSpec{name: "alpha", supervisorPort: blocked}))
	resp := f.reload()

	assertDiff(t, resp, "", "", "alpha", "")
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0], "keeping its previous registration") {
		t.Errorf("Errors = %v, want the kept-registration error", resp.Errors)
	}
	st := projectStatusByName(t, f.socket, "alpha")
	if st.SupervisorPort != oldPort || st.State != StateStopped {
		t.Errorf("alpha = port %d state %q, want the previous port %d, stopped", st.SupervisorPort, st.State, oldPort)
	}
	if code, _ := getPort(t, oldPort, "/"); code != http.StatusOK {
		t.Errorf("previous port %d = %d, want 200 (cold start on the kept registration)", oldPort, code)
	}

	// Port freed: the next reload sees the project as changed again.
	if err := blocker.Close(); err != nil {
		t.Fatal(err)
	}
	resp = f.reload()
	assertDiff(t, resp, "", "", "alpha", "")
	if len(resp.Errors) != 0 {
		t.Errorf("Errors = %v, want none", resp.Errors)
	}
	if code, _ := getPort(t, blocked, "/"); code != http.StatusOK {
		t.Errorf("new port %d = %d, want 200", blocked, code)
	}
	if portAccepts(oldPort) {
		t.Errorf("previous port %d still accepts connections", oldPort)
	}
}

// TestReloadRequestDuringChangeWaitsForReplacement: a request that arrives
// while a changed project's old process is still being stopped (a
// SIGTERM-ignoring process makes the stop take the full shutdown timeout)
// is neither refused nor sent to the dying process — it waits, then is
// served by the new configuration.
func TestReloadRequestDuringChangeWaitsForReplacement(t *testing.T) {
	f := newReloadFixture(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha", mode: testproc.ModeHTTPStubborn, shutdownSecs: 2}))
	f.start()
	_, _, pid1 := f.get("alpha", "/")

	f.writeMain(f.yaml(projectSpec{name: "alpha", commandPrefix: "echo replaced", shutdownSecs: 2}))
	type outcome struct {
		resp *control.ReloadResponse
	}
	reloadDone := make(chan outcome, 1)
	go func() { reloadDone <- outcome{f.reload()} }()

	// Land the request while the old process is still being stopped.
	time.Sleep(500 * time.Millisecond)
	if st := projectStatusByName(t, f.socket, "alpha"); st.State != StateStopping {
		t.Fatalf("alpha mid-reload = %q, want %q (the stubborn process should still be draining)", st.State, StateStopping)
	}
	started := time.Now()
	code, body, pid2 := f.get("alpha", "/during-reload")
	if code != http.StatusOK {
		t.Fatalf("request during reload = %d; body:\n%s", code, body)
	}
	if pid2 == pid1 {
		t.Errorf("request during reload answered by the old process %d", pid1)
	}
	if waited := time.Since(started); waited < time.Second {
		t.Errorf("request was answered after %s; it should have waited for the old process to drain", waited)
	}

	select {
	case out := <-reloadDone:
		assertDiff(t, out.resp, "", "", "alpha", "")
	case <-time.After(30 * time.Second):
		t.Fatal("reload never finished")
	}
}

// TestSIGHUPReloadsConfig: SIGHUP triggers the same reload path.
func TestSIGHUPReloadsConfig(t *testing.T) {
	f := newReloadFixture(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha"}))
	f.start()

	f.writeFragment("beta.yaml", f.yaml(projectSpec{name: "beta"}))
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := f.client.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !status.LastReloadAt.IsZero() && len(status.Projects) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SIGHUP did not reload the config; status = %+v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, _, _ := f.get("beta", "/"); code != http.StatusOK {
		t.Errorf("beta after SIGHUP reload = %d, want 200", code)
	}
}

// TestReloadWithoutConfigFileIsRejected: a daemon built from an in-memory
// config has nothing to re-read.
func TestReloadWithoutConfigFileIsRejected(t *testing.T) {
	socket, _, _ := startDaemon(t, testConfig(freePort(t), freePort(t)))

	_, err := control.NewClient(socket).Reload(context.Background())

	var apiErr *control.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 || !strings.Contains(apiErr.Message, "in-memory") {
		t.Errorf("Reload without a config file = %v, want a 500 explaining there is no file", err)
	}
}

// TestReloadRemovedFragmentRemovesProject: deleting a projects.d file
// removes its project on the next reload.
func TestReloadRemovedFragmentRemovesProject(t *testing.T) {
	f := newReloadFixture(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha"}))
	f.writeFragment("beta.yaml", f.yaml(projectSpec{name: "beta"}))
	f.start()

	f.removeFragment("beta.yaml")
	resp := f.reload()

	assertDiff(t, resp, "", "beta", "", "alpha")
	if portAccepts(f.supervisorPort("beta")) {
		t.Error("beta's port still accepts connections after its file was removed")
	}
}
