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
package e2e

import (
	"fmt"
	"net/http"
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
