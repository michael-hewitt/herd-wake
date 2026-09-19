// End-to-end acceptance tests for the spec's §18 criteria that are
// automatable without Laravel Herd (see issue #1). Each test starts its own
// daemon subprocess against a fresh config and drives it through supervisor
// ports, the CLI, and the control API.
//
//	§18.2  TestColdStartServesViteIndex
//	§18.3  TestConcurrentColdStartsShareOneProcess
//	§18.4  TestWarmRequestOverhead
//	§18.5+6 TestHMRWebSocketKeepsProjectRunning (HTTPS termination itself is Herd's job)
//	§18.7  TestIdleStopAndRevive
//	§18.8  TestProjectIsolation
//	§18.9  TestFailedStartupDiagnostic
//	§18.10 TestDaemonRestartLeavesProjectsStopped
//	#14    TestWildcardWorktreesServedByHost
//	#11    TestPlainNodeServerCommand
//	#12    TestPreviewForwardedHeadersBehindNginx
//	#12    TestPreviewEnvFileAndSafeDiagnostics
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// fixtureMarker is served by the fixture's index.html.
const fixtureMarker = "herd-wake-vite-fixture"

// §18.2: visiting a stopped project's URL starts the correct server and
// eventually returns the requested page.
func TestColdStartServesViteIndex(t *testing.T) {
	requireE2E(t)
	ports := freePorts(t, 2)
	d := startDaemon(t, viteProject("vite-cold", ports[0], ports[1], 0))

	if state := d.projectStatus("vite-cold").State; state != "stopped" {
		t.Fatalf("fresh project state = %q, want stopped", state)
	}

	code, body := get(t, ports[0], "/")
	if code != http.StatusOK {
		t.Fatalf("cold GET status = %d, want 200; body:\n%s", code, body)
	}
	if !strings.Contains(body, fixtureMarker) {
		t.Fatalf("cold GET body does not contain %q:\n%s", fixtureMarker, body)
	}
	if !strings.Contains(body, "/@vite/client") {
		t.Fatalf("cold GET body is not Vite-transformed (no /@vite/client):\n%s", body)
	}

	status := d.projectStatus("vite-cold")
	if status.State != "running" || status.PID == 0 {
		t.Fatalf("after cold start: state=%q pid=%d, want running with a pid", status.State, status.PID)
	}
}

// §18.3: concurrent cold-start requests create only one dev-server process.
func TestConcurrentColdStartsShareOneProcess(t *testing.T) {
	requireE2E(t)
	ports := freePorts(t, 2)
	d := startDaemon(t, viteProject("vite-flock", ports[0], ports[1], 0))

	const concurrency = 20
	errs := make(chan error, concurrency)
	var wg sync.WaitGroup
	for i := range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := supervisorClient.Get(fmt.Sprintf("http://127.0.0.1:%d/?req=%d", ports[0], i))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close() //nolint:errcheck // status is all we need
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("request %d: status %d", i, resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent cold start: %v", err)
	}

	if n := countViteProcs(t, ports[1]); n != 1 {
		t.Fatalf("vite process count after %d concurrent cold-start requests = %d, want exactly 1", concurrency, n)
	}
	pid := d.projectStatus("vite-flock").PID
	if pid == 0 {
		t.Fatal("project has no pid after concurrent cold start")
	}
	if code, _ := get(t, ports[0], "/"); code != http.StatusOK {
		t.Fatalf("follow-up GET status = %d, want 200", code)
	}
	if again := d.projectStatus("vite-flock").PID; again != pid {
		t.Fatalf("pid changed from %d to %d: project was restarted", pid, again)
	}
}

// §18.4: requests to a running project incur negligible supervisor overhead.
// The threshold is a CI-safe sanity bound, not a benchmark.
func TestWarmRequestOverhead(t *testing.T) {
	requireE2E(t)
	ports := freePorts(t, 2)
	_ = startDaemon(t, viteProject("vite-warm", ports[0], ports[1], 0))

	if code, _ := get(t, ports[0], "/"); code != http.StatusOK {
		t.Fatalf("cold GET status = %d, want 200", code)
	}

	const samples = 30
	durations := make([]time.Duration, 0, samples)
	for range samples {
		start := time.Now()
		code, _ := get(t, ports[0], "/")
		if code != http.StatusOK {
			t.Fatalf("warm GET status = %d, want 200", code)
		}
		durations = append(durations, time.Since(start))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	median := durations[samples/2]
	t.Logf("warm request latency: p50=%s min=%s max=%s", median, durations[0], durations[samples-1])
	if median > 500*time.Millisecond {
		t.Fatalf("warm request p50 = %s, want < 500ms", median)
	}
}

// §18.5 partially + §18.6: Vite HMR connects through the supervisor, and an
// open HMR WebSocket keeps the project running past its idle timeout.
// (The HTTPS half of §18.5 is Herd's TLS termination and needs Herd itself.)
func TestHMRWebSocketKeepsProjectRunning(t *testing.T) {
	requireE2E(t)
	const idleSeconds = 2
	ports := freePorts(t, 2)
	d := startDaemon(t, viteProject("vite-hmr", ports[0], ports[1], idleSeconds))

	if code, _ := get(t, ports[0], "/"); code != http.StatusOK {
		t.Fatal("cold GET failed")
	}

	// Vite's HMR endpoint shares the dev-server port and requires the
	// vite-hmr subprotocol; connect through the supervisor port.
	addr := fmt.Sprintf("127.0.0.1:%d", ports[0])
	ws, err := testproc.DialWSProtocol(addr, addr, "/", "vite-hmr", time.Minute)
	if err != nil {
		t.Fatalf("dial HMR websocket through supervisor: %v", err)
	}
	defer ws.Abort() //nolint:errcheck // best-effort teardown on failure paths
	if err := ws.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	greeting, err := ws.ReadText()
	if err != nil {
		t.Fatalf("read HMR greeting: %v", err)
	}
	if !strings.Contains(greeting, "connected") {
		t.Fatalf("HMR greeting = %q, want a vite \"connected\" message", greeting)
	}

	// Well past the idle timeout, the open socket must keep it running.
	time.Sleep(3 * idleSeconds * time.Second)
	if state := d.projectStatus("vite-hmr").State; state != "running" {
		t.Fatalf("project state with open HMR socket = %q, want running", state)
	}

	// Closing the last socket starts a fresh idle window; the project then
	// stops on its own.
	if err := ws.Close(); err != nil {
		t.Logf("websocket close: %v (continuing)", err)
	}
	d.waitForState("vite-hmr", "stopped", 30*time.Second)
}

// §18.7: a project stops after its idle timeout with no traffic, and the
// next request revives it.
func TestIdleStopAndRevive(t *testing.T) {
	requireE2E(t)
	const idleSeconds = 2
	ports := freePorts(t, 2)
	d := startDaemon(t, viteProject("vite-idle", ports[0], ports[1], idleSeconds))

	if code, _ := get(t, ports[0], "/"); code != http.StatusOK {
		t.Fatal("cold GET failed")
	}
	firstPID := d.projectStatus("vite-idle").PID
	if firstPID == 0 {
		t.Fatal("no pid after cold start")
	}

	d.waitForState("vite-idle", "stopped", 30*time.Second)
	if n := countViteProcs(t, ports[1]); n != 0 {
		t.Fatalf("vite process count after idle stop = %d, want 0", n)
	}

	code, body := get(t, ports[0], "/")
	if code != http.StatusOK || !strings.Contains(body, fixtureMarker) {
		t.Fatalf("revive GET status = %d, want 200 with fixture body", code)
	}
	revived := d.projectStatus("vite-idle")
	if revived.State != "running" {
		t.Fatalf("state after revive = %q, want running", revived.State)
	}
	if revived.PID == firstPID {
		t.Fatalf("revived pid %d equals the stopped process's pid", firstPID)
	}
}

// §18.8: stopping one project leaves the other serving, and the stopped one
// wakes again on demand.
func TestProjectIsolation(t *testing.T) {
	requireE2E(t)
	ports := freePorts(t, 4)
	d := startDaemon(t,
		viteProject("vite-a", ports[0], ports[1], 0)+
			nodeProject(t, "node-b", ports[2], ports[3], 0))

	if code, _ := get(t, ports[0], "/"); code != http.StatusOK {
		t.Fatal("cold GET to vite-a failed")
	}
	if code, body := get(t, ports[2], "/"); code != http.StatusOK || !strings.Contains(body, "hello from node-b") {
		t.Fatalf("cold GET to node-b: status %d body %q", code, body)
	}

	out, err := d.cli("project:stop", "vite-a")
	if err != nil {
		t.Fatalf("herd-wake project:stop vite-a: %v\n%s", err, out)
	}
	d.waitForState("vite-a", "stopped", 15*time.Second)
	if n := countViteProcs(t, ports[1]); n != 0 {
		t.Fatalf("vite process count after project:stop = %d, want 0", n)
	}

	// The other project keeps serving without interruption.
	if code, body := get(t, ports[2], "/"); code != http.StatusOK || !strings.Contains(body, "hello from node-b") {
		t.Fatalf("node-b after stopping vite-a: status %d body %q", code, body)
	}
	if state := d.projectStatus("node-b").State; state != "running" {
		t.Fatalf("node-b state = %q, want running", state)
	}

	// And the stopped project cold-starts again on demand.
	if code, _ := get(t, ports[0], "/"); code != http.StatusOK {
		t.Fatal("vite-a did not revive after project:stop")
	}
}

// §18.9: a project whose command fails yields a 503 diagnostic (with the
// process's own output) without destabilizing the daemon or other projects.
func TestFailedStartupDiagnostic(t *testing.T) {
	requireE2E(t)
	ports := freePorts(t, 4)
	d := startDaemon(t,
		brokenProject(t, "broken", ports[0], ports[1])+
			nodeProject(t, "healthy", ports[2], ports[3], 0))

	code, body := get(t, ports[0], "/")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("GET to broken project: status %d, want 503; body:\n%s", code, body)
	}
	if !strings.Contains(body, `project "broken" is unavailable`) {
		t.Fatalf("503 body is not the herd-wake diagnostic:\n%s", body)
	}
	if !strings.Contains(body, "this-script-does-not-exist.js") {
		t.Fatalf("503 diagnostic does not include the process's own output:\n%s", body)
	}
	if state := d.projectStatus("broken").State; state != "failed" {
		t.Fatalf("broken project state = %q, want failed", state)
	}

	// While the failure is in its retry-backoff window, requests still get a
	// prompt 503 instead of hammering the broken command.
	if code, _ := get(t, ports[0], "/"); code != http.StatusServiceUnavailable {
		t.Fatalf("GET during backoff: status %d, want 503", code)
	}

	// The daemon keeps serving the healthy project.
	if code, respBody := get(t, ports[2], "/"); code != http.StatusOK || !strings.Contains(respBody, "hello from healthy") {
		t.Fatalf("healthy project alongside a failed one: status %d body %q", code, respBody)
	}
}

// §18.10: restarting the daemon restores no previously-running projects;
// everything (non-always_on) comes back stopped and wakes only on demand.
func TestDaemonRestartLeavesProjectsStopped(t *testing.T) {
	requireE2E(t)
	ports := freePorts(t, 2)
	projectYAML := viteProject("vite-restart", ports[0], ports[1], 0)

	d := startDaemon(t, projectYAML)
	if code, _ := get(t, ports[0], "/"); code != http.StatusOK {
		t.Fatal("cold GET failed")
	}
	if state := d.projectStatus("vite-restart").State; state != "running" {
		t.Fatalf("state before daemon shutdown = %q, want running", state)
	}

	d.stopGracefully()
	if n := countViteProcs(t, ports[1]); n != 0 {
		t.Fatalf("vite process count after daemon shutdown = %d, want 0 (daemon must stop its children)", n)
	}

	d2 := startDaemon(t, projectYAML)
	time.Sleep(2 * time.Second) // any (wrong) auto-start would begin immediately
	if state := d2.projectStatus("vite-restart").State; state != "stopped" {
		t.Fatalf("state after daemon restart = %q, want stopped (no auto-start)", state)
	}
	if n := countViteProcs(t, ports[1]); n != 0 {
		t.Fatalf("vite process count after daemon restart = %d, want 0", n)
	}

	// It still wakes on demand under the new daemon.
	if code, body := get(t, ports[0], "/"); code != http.StatusOK || !strings.Contains(body, fixtureMarker) {
		t.Fatalf("GET after daemon restart: status %d", code)
	}
}

// Wildcard discovery (issue #14): one listener for a folder of worktrees,
// each served at <label>.<base_domain> on demand and told apart by Host;
// a label that names no servable worktree is a 404 diagnostic.
func TestWildcardWorktreesServedByHost(t *testing.T) {
	requireE2E(t)
	ports := freePorts(t, 3)
	workspace := t.TempDir()
	server := `const http = require('http');
const port = Number(require('fs').readFileSync('port', 'utf8'));
http.createServer((req, res) => { res.end('hello from ' + require('path').basename(process.cwd()) + ' host=' + req.headers.host); })
  .listen(port, '127.0.0.1');
`
	for label, port := range map[string]int{"alpha": ports[1], "beta": ports[2]} {
		dir := filepath.Join(workspace, label)
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "server.js"), []byte(server), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "port"), []byte(fmt.Sprint(port)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d := startDaemon(t, fmt.Sprintf(`discovery:
  - name: e2e
    mode: wildcard
    base_domain: e2e.test
    supervisor_port: %d
    directory: %s
    require_files: [server.js]
    port_command: cat port
    command: node server.js
    startup_timeout_seconds: 60
`, ports[0], workspace))

	for _, label := range []string{"alpha", "beta"} {
		code, body := getHost(t, ports[0], label+".e2e.test", "/")
		if code != http.StatusOK || !strings.Contains(body, "hello from "+label) || !strings.Contains(body, "host="+label+".e2e.test") {
			t.Fatalf("GET %s.e2e.test: status %d body %q", label, code, body)
		}
		if st := d.projectStatus(label); st.State != "running" || !st.Dynamic || st.Source != "discovery:e2e" {
			t.Fatalf("%s status = %+v, want a running dynamic project", label, st)
		}
	}
	if code, body := getHost(t, ports[0], "nope.e2e.test", "/"); code != http.StatusNotFound || !strings.Contains(body, `no worktree named "nope"`) {
		t.Fatalf("GET nope.e2e.test: status %d body %q", code, body)
	}
	out, err := d.cli("project:stop", "alpha")
	if err != nil {
		t.Fatalf("herd-wake project:stop alpha: %v\n%s", err, out)
	}
	d.waitForState("alpha", "stopped", 15*time.Second)
	if code, _ := getHost(t, ports[0], "beta.e2e.test", "/"); code != http.StatusOK {
		t.Fatalf("beta after stopping alpha: status %d", code)
	}
}

// Plain node command (issue #11): a project whose command is a bare `node
// server.js` with the port supplied through `env: {PORT: …}` rather than an
// npm script — the staging-server shape — cold-starts through its
// supervisor port, stops gracefully on project:stop (the fixture exits 0 on
// SIGTERM, well inside the shutdown timeout, so no SIGKILL is involved), and
// cold-starts again on the next request.
func TestPlainNodeServerCommand(t *testing.T) {
	requireE2E(t)
	const shutdownTimeoutSeconds = 30
	ports := freePorts(t, 2)
	d := startDaemon(t, nodeFixtureProject("node-plain", ports[0], ports[1], shutdownTimeoutSeconds))

	if state := d.projectStatus("node-plain").State; state != "stopped" {
		t.Fatalf("fresh project state = %q, want stopped", state)
	}

	wantBody := fmt.Sprintf("node-fixture ok port=%d", ports[1])
	code, body := get(t, ports[0], "/")
	if code != http.StatusOK || !strings.Contains(body, wantBody) {
		t.Fatalf("cold GET: status %d body %q, want 200 containing %q", code, body, wantBody)
	}
	first := d.projectStatus("node-plain")
	if first.State != "running" || first.PID == 0 {
		t.Fatalf("after cold start: state=%q pid=%d, want running with a pid", first.State, first.PID)
	}
	if n := countNodeFixtureProcs(t); n != 1 {
		t.Fatalf("node-fixture process count after cold start = %d, want 1", n)
	}

	// Graceful stop: the fixture must be gone long before the daemon would
	// escalate to SIGKILL.
	stopped := time.Now()
	out, err := d.cli("project:stop", "node-plain")
	if err != nil {
		t.Fatalf("herd-wake project:stop node-plain: %v\n%s", err, out)
	}
	d.waitForState("node-plain", "stopped", 10*time.Second)
	if took := time.Since(stopped); took >= shutdownTimeoutSeconds*time.Second {
		t.Fatalf("project:stop took %s, at or past the %ds shutdown timeout: the server did not exit on SIGTERM", took, shutdownTimeoutSeconds)
	}
	if n := countNodeFixtureProcs(t); n != 0 {
		t.Fatalf("node-fixture process count after project:stop = %d, want 0", n)
	}

	// The next request cold-starts a fresh process.
	code, body = get(t, ports[0], "/")
	if code != http.StatusOK || !strings.Contains(body, wantBody) {
		t.Fatalf("revive GET: status %d body %q, want 200 containing %q", code, body, wantBody)
	}
	revived := d.projectStatus("node-plain")
	if revived.State != "running" || revived.PID == 0 {
		t.Fatalf("after revive: state=%q pid=%d, want running with a pid", revived.State, revived.PID)
	}
	if revived.PID == first.PID {
		t.Fatalf("revived pid %d equals the stopped process's pid", first.PID)
	}
}

// getHost performs GET http://127.0.0.1:port/path with an explicit Host
// header, as Herd would send it for a wildcard site.
func getHost(t *testing.T, port int, host, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := supervisorClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", host, err)
	}
	defer resp.Body.Close() //nolint:errcheck // body fully read below
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// getHostHeaders performs GET http://127.0.0.1:port/path with an explicit
// Host header and any extra request headers, the shape a front proxy puts
// on the wire when it forwards to a herd-wake listener.
func getHostHeaders(t *testing.T, port int, host, path string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := supervisorClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", host, err)
	}
	defer resp.Body.Close() //nolint:errcheck // body fully read below
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// writeWorktree creates <workspace>/<label> as a servable worktree: a .git
// directory so it looks like one, the given server.js, and a port file for
// the entry's `port_command: cat port`. It returns the worktree's path so
// callers can drop further files (an env file) into it.
func writeWorktree(t *testing.T, workspace, label, serverJS string, port int) string {
	t.Helper()
	dir := filepath.Join(workspace, label)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.js"), []byte(serverJS), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "port"), []byte(fmt.Sprint(port)), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Forwarded headers behind nginx (issue #12): a branch server reached
// through a wildcard listener must see what the README's preview-box nginx
// block sends — the public host, the real client appended to
// X-Forwarded-For with herd-wake's own hop, and X-Forwarded-Proto: https so
// the app generates https URLs. The request carries exactly the block's
// headers for client 203.0.113.9 over TLS, including the unconditional
// `Connection: upgrade` whose empty $http_upgrade drops the Upgrade header,
// which must still be handled as an ordinary request.
func TestPreviewForwardedHeadersBehindNginx(t *testing.T) {
	requireE2E(t)
	ports := freePorts(t, 2)
	workspace := t.TempDir()
	const host = "alpha.fwd.test"
	writeWorktree(t, workspace, "alpha", `const http = require('http');
const port = Number(require('fs').readFileSync('port', 'utf8'));
http.createServer((req, res) => {
  res.setHeader('content-type', 'application/json');
  res.end(JSON.stringify({
    host: req.headers['host'] || '',
    forwarded_for: req.headers['x-forwarded-for'] || '',
    forwarded_proto: req.headers['x-forwarded-proto'] || '',
    forwarded_host: req.headers['x-forwarded-host'] || '',
    upgrade: req.headers['upgrade'] || '',
  }));
}).listen(port, '127.0.0.1');
`, ports[1])

	startDaemon(t, fmt.Sprintf(`discovery:
  - name: fwd
    mode: wildcard
    base_domain: fwd.test
    supervisor_port: %d
    directory: %s
    require_files: [server.js]
    port_command: cat port
    command: node server.js
    startup_timeout_seconds: 60
`, ports[0], workspace))

	code, body := getHostHeaders(t, ports[0], host, "/", map[string]string{
		"X-Forwarded-For":   "203.0.113.9",
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  host,
		"Connection":        "upgrade", // nginx sets it unconditionally; $http_upgrade was empty
	})
	if code != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200; body:\n%s", host, code, body)
	}

	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("branch server body is not the header dump: %v\nbody: %q", err, body)
	}
	want := map[string]string{
		"host":            host,
		"forwarded_for":   "203.0.113.9, 127.0.0.1",
		"forwarded_proto": "https",
		"forwarded_host":  host,
		"upgrade":         "",
	}
	for _, key := range []string{"host", "forwarded_for", "forwarded_proto", "forwarded_host", "upgrade"} {
		if got[key] != want[key] {
			t.Errorf("branch server saw %s = %q, want %q", key, got[key], want[key])
		}
	}
}

// The preview-box scenario (issue #12): a wildcard entry over a folder of
// prepped branches, its secrets in env files rather than the config, and
// `diagnostic_logs: false` so the pages a reviewer can reach over the
// internet say nothing private. It asserts both halves of that bargain —
// the branch server really receives the shared and per-worktree variables,
// and the database password reaches nothing public: not a 503 (plain or
// HTML), not `status --json`, not `herd-wake projects`, not the daemon's
// own log — while `herd-wake logs`, the operator's view on the host, stays
// complete. A world-readable shared env file must also earn a warning.
func TestPreviewEnvFileAndSafeDiagnostics(t *testing.T) {
	requireE2E(t)
	const secret = "hunter2"
	const dbLogin = "postgres://syp:" + secret + "@localhost/staging"
	ports := freePorts(t, 3)
	workspace := t.TempDir()

	// The shared template file, deliberately 0644 so the daemon's
	// permission warning is exercised; the per-worktree file is 0600 as the
	// README tells operators to write it.
	sharedEnv := filepath.Join(t.TempDir(), "preview.env")
	if err := os.WriteFile(sharedEnv, []byte("DB_LOGIN="+dbLogin+"\nENVIRONMENT=preview\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	alpha := writeWorktree(t, workspace, "alpha", `const http = require('http');
const port = Number(require('fs').readFileSync('port', 'utf8'));
http.createServer((req, res) => {
  res.setHeader('content-type', 'application/json');
  res.end(JSON.stringify({
    DB_LOGIN: process.env.DB_LOGIN || '',
    ENVIRONMENT: process.env.ENVIRONMENT || '',
    SUPPRESS_DB_TESTS: process.env.SUPPRESS_DB_TESTS || '',
    BRANCH_DB: process.env.BRANCH_DB || '',
  }));
}).listen(port, '127.0.0.1');
`, ports[1])
	if err := os.WriteFile(filepath.Join(alpha, ".env.preview"),
		[]byte("SUPPRESS_DB_TESTS=1\nBRANCH_DB=staging_copy_alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A branch whose server dies at once, having printed the secret it was
	// given: the worst case for a public diagnostic.
	writeWorktree(t, workspace, "broken", `process.stderr.write('DB_LOGIN is ' + process.env.DB_LOGIN + '\n');
process.exit(1);
`, ports[2])

	d := startDaemon(t, fmt.Sprintf(`discovery:
  - name: preview
    mode: wildcard
    base_domain: preview.test
    supervisor_port: %d
    directory: %s
    require_files: [server.js]
    port_command: cat port
    command: node server.js
    env_file:
      - %s
      - .env.preview
    diagnostic_logs: false
    startup_timeout_seconds: 60
`, ports[0], workspace, sharedEnv))

	// The healthy branch receives both files' variables.
	code, body := getHost(t, ports[0], "alpha.preview.test", "/")
	if code != http.StatusOK {
		t.Fatalf("GET alpha.preview.test: status %d, want 200; body:\n%s", code, body)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("alpha body is not the env dump: %v\nbody: %q", err, body)
	}
	want := map[string]string{
		"DB_LOGIN":          dbLogin,
		"ENVIRONMENT":       "preview",
		"SUPPRESS_DB_TESTS": "1",
		"BRANCH_DB":         "staging_copy_alpha",
	}
	for _, name := range []string{"DB_LOGIN", "ENVIRONMENT", "SUPPRESS_DB_TESTS", "BRANCH_DB"} {
		if got[name] != want[name] {
			t.Errorf("branch server received %s = %q, want %q", name, got[name], want[name])
		}
	}

	// Starting it warned about the shared file's mode, without quoting it.
	if out := d.output.String(); !strings.Contains(out, "readable by other users") {
		t.Errorf("daemon output has no permission warning for the 0644 shared env file:\n%s", out)
	}

	// The broken branch's 503, in both renderings, is terse: it names the
	// branch and where the operator reads its output, and nothing else.
	for _, tt := range []struct {
		name    string
		headers map[string]string
	}{
		{name: "plain text"},
		{name: "html", headers: map[string]string{"Accept": "text/html"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, body := getHostHeaders(t, ports[0], "broken.preview.test", "/", tt.headers)
			if code != http.StatusServiceUnavailable {
				t.Fatalf("GET broken.preview.test: status %d, want 503; body:\n%s", code, body)
			}
			for _, want := range []string{"broken", "diagnostic_logs: false", "herd-wake logs broken"} {
				if !strings.Contains(body, want) {
					t.Errorf("503 body does not contain %q:\n%s", want, body)
				}
			}
			for _, unwanted := range []string{secret, "DB_LOGIN is"} {
				if strings.Contains(body, unwanted) {
					t.Errorf("503 body leaks %q:\n%s", unwanted, body)
				}
			}
		})
	}

	// A label with no worktree reveals neither the workspace nor a path.
	code, body = getHost(t, ports[0], "nope.preview.test", "/")
	if code != http.StatusNotFound {
		t.Fatalf("GET nope.preview.test: status %d, want 404; body:\n%s", code, body)
	}
	if strings.Contains(body, workspace) {
		t.Errorf("404 body leaks the workspace path %q:\n%s", workspace, body)
	}

	// On the host, the operator still sees everything the branch printed.
	logs, err := d.cli("logs", "broken")
	if err != nil {
		t.Fatalf("herd-wake logs broken: %v\n%s", err, logs)
	}
	if !strings.Contains(logs, secret) {
		t.Errorf("herd-wake logs broken does not show the process output:\n%s", logs)
	}

	// Nothing an operator might paste or share carries the password.
	statusJSON, err := d.cli("status", "--json")
	if err != nil {
		t.Fatalf("herd-wake status --json: %v\n%s", err, statusJSON)
	}
	if strings.Contains(statusJSON, secret) {
		t.Errorf("herd-wake status --json leaks the env-file value:\n%s", statusJSON)
	}
	projects, err := exec.Command(binPath, "projects", "--config", d.configPath).CombinedOutput() //nolint:gosec // test binary + fixed args
	if err != nil {
		t.Fatalf("herd-wake projects: %v\n%s", err, projects)
	}
	if strings.Contains(string(projects), secret) {
		t.Errorf("herd-wake projects leaks the env-file value:\n%s", projects)
	}
	if out := d.output.String(); strings.Contains(out, secret) {
		t.Errorf("daemon output leaks the env-file value:\n%s", out)
	}
}
