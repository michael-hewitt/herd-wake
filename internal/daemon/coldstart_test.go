package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// httpProject builds a one-project config whose command re-invokes the test
// binary as an HTTP echo server (testproc.ModeHTTP) on the application port.
func httpProject(t *testing.T) *config.Config {
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
			HoldMaxWaitSeconds:     15,
			HoldMaxRequests:        100,
			Env: map[string]string{
				testproc.EnvMode: testproc.ModeHTTP,
				testproc.EnvPort: strconv.Itoa(applicationPort),
			},
		},
	}}
}

// crashProject builds a one-project config whose command crashes before ever
// becoming ready.
func crashProject(t *testing.T, command string) *config.Config {
	t.Helper()
	cfg := httpProject(t)
	p := cfg.Projects["dashboard"]
	p.Command = command
	p.Env = nil
	p.StartupTimeoutSeconds = 5
	return cfg
}

// supervisorGet performs one request against the project's supervisor port.
func supervisorGet(t *testing.T, cfg *config.Config, path string, header http.Header) (*http.Response, string) {
	t.Helper()
	port := cfg.Projects["dashboard"].SupervisorPort
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "dashboard.test"
	for k, vs := range header {
		req.Header[k] = vs
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through supervisor port: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, string(body)
}

// countSpawns counts how many times the project's process printed its
// startup marker, i.e. how many processes were actually spawned.
func countSpawns(t *testing.T, socket, marker string) int {
	t.Helper()
	logs, err := control.NewClient(socket).Logs(context.Background(), "dashboard", 0)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	count := 0
	for _, line := range logs.Lines {
		if strings.Contains(line, marker) {
			count++
		}
	}
	return count
}

var pidPattern = regexp.MustCompile(`pid=(\d+)`)

// TestColdStartViaRequest is the core acceptance path: a request to a
// stopped project starts it, is held while it starts, and receives the
// correct response from the now-running server. A follow-up request rides
// the hot path against the same process.
func TestColdStartViaRequest(t *testing.T) {
	cfg := httpProject(t)
	socket, _, _ := startDaemon(t, cfg)

	if state := statusState(t, socket); state != StateStopped {
		t.Fatalf("initial state = %q, want %q", state, StateStopped)
	}

	resp, body := supervisorGet(t, cfg, "/hello?x=1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cold-start status = %d, want 200; body:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "ok GET /hello host=dashboard.test") {
		t.Errorf("cold-start body = %q, want the upstream echo", body)
	}
	pid := pidPattern.FindStringSubmatch(body)
	if pid == nil {
		t.Fatalf("cold-start body missing pid marker: %q", body)
	}

	// Hot path: the same process answers again.
	resp2, body2 := supervisorGet(t, cfg, "/again", nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("hot-path status = %d, want 200; body:\n%s", resp2.StatusCode, body2)
	}
	pid2 := pidPattern.FindStringSubmatch(body2)
	if pid2 == nil || pid2[1] != pid[1] {
		t.Errorf("hot-path pid = %v, want the cold-start process %v (no respawn)", pid2, pid)
	}
	if state := statusState(t, socket); state != StateRunning {
		t.Errorf("state after cold start = %q, want %q", state, StateRunning)
	}
}

// statusState fetches the project's lifecycle state over the control socket.
func statusState(t *testing.T, socket string) string {
	t.Helper()
	status, err := control.NewClient(socket).Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return status.Projects[0].State
}

// TestConcurrentColdStartsShareOneProcess fires many requests at a stopped
// (and deliberately slow-starting) project: exactly one process may be
// spawned, and every request must be answered by it.
func TestConcurrentColdStartsShareOneProcess(t *testing.T) {
	cfg := httpProject(t)
	cfg.Projects["dashboard"].Env[testproc.EnvStartDelay] = "300ms"
	socket, _, _ := startDaemon(t, cfg)

	const requests = 24
	type result struct {
		status int
		body   string
	}
	results := make([]result, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port := cfg.Projects["dashboard"].SupervisorPort
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/n/%d", port, i))
			if err != nil {
				results[i] = result{status: -1, body: err.Error()}
				return
			}
			defer resp.Body.Close() //nolint:errcheck // test cleanup
			body, _ := io.ReadAll(resp.Body)
			results[i] = result{status: resp.StatusCode, body: string(body)}
		}()
	}
	wg.Wait()

	pids := map[string]bool{}
	for i, res := range results {
		if res.status != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200; body: %s", i, res.status, res.body)
			continue
		}
		if !strings.Contains(res.body, fmt.Sprintf("ok GET /n/%d ", i)) {
			t.Errorf("request %d got someone else's response: %q", i, res.body)
		}
		if m := pidPattern.FindStringSubmatch(res.body); m != nil {
			pids[m[1]] = true
		}
	}
	if len(pids) != 1 {
		t.Errorf("responses came from %d distinct processes %v, want exactly 1", len(pids), pids)
	}
	if spawns := countSpawns(t, socket, "testproc serving http"); spawns != 1 {
		t.Errorf("process spawned %d times, want exactly 1", spawns)
	}
}

// TestFailedStartReturns503Diagnostic covers startup failure: the held
// request gets a 503 whose body includes the project name, its state, the
// exit summary, and recent process output — plain text for API clients,
// an HTML page when the Accept header prefers text/html — and the daemon
// itself stays healthy.
func TestFailedStartReturns503Diagnostic(t *testing.T) {
	cfg := crashProject(t, "echo boom-diagnostic; echo second-line >&2; exit 3")
	socket, _, _ := startDaemon(t, cfg)

	resp, body := supervisorGet(t, cfg, "/", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body:\n%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain for a non-browser client", ct)
	}
	for _, want := range []string{`"dashboard"`, "failed", "exit status 3", "boom-diagnostic", "second-line"} {
		if !strings.Contains(body, want) {
			t.Errorf("plain 503 diagnostic missing %q; got:\n%s", want, body)
		}
	}

	// A browser request (this one lands in the retry backoff window) gets an
	// HTML page with the same diagnostics.
	resp, body = supervisorGet(t, cfg, "/", http.Header{"Accept": []string{"text/html,application/xhtml+xml"}})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("browser status = %d, want 503; body:\n%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html for a browser client", ct)
	}
	for _, want := range []string{"<!DOCTYPE html", "dashboard", "boom-diagnostic"} {
		if !strings.Contains(body, want) {
			t.Errorf("HTML 503 diagnostic missing %q; got:\n%s", want, body)
		}
	}

	// The daemon is not destabilized: the control socket still answers.
	if state := statusState(t, socket); state != StateFailed {
		t.Errorf("state = %q, want %q", state, StateFailed)
	}
}

// TestCrashLoopRespectsBackoffEndToEnd verifies that request traffic cannot
// hot-loop a crashing project: requests inside the backoff window are
// answered 503 without a respawn, a manual project:start retries (and fails)
// immediately, and once the backoff window elapses a request retries again.
func TestCrashLoopRespectsBackoffEndToEnd(t *testing.T) {
	cfg := crashProject(t, "echo spawn-marker; exit 1")
	socket, _, _ := startDaemon(t, cfg)
	client := control.NewClient(socket)

	// First request triggers the first (failing) startup.
	resp, _ := supervisorGet(t, cfg, "/", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if spawns := countSpawns(t, socket, "spawn-marker"); spawns != 1 {
		t.Fatalf("spawns after first request = %d, want 1", spawns)
	}

	// Requests during the backoff window are refused without a respawn.
	for i := 0; i < 3; i++ {
		resp, body := supervisorGet(t, cfg, "/", nil)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("backoff request %d: status = %d, want 503; body:\n%s", i, resp.StatusCode, body)
		}
	}
	if spawns := countSpawns(t, socket, "spawn-marker"); spawns != 1 {
		t.Errorf("spawns after backoff-window requests = %d, want still 1 (no hot loop)", spawns)
	}

	// Manual project:start bypasses the backoff and retries immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := client.StartProject(ctx, "dashboard"); err == nil {
		t.Fatal("StartProject on a crashing command should report the failure")
	}
	if spawns := countSpawns(t, socket, "spawn-marker"); spawns != 2 {
		t.Errorf("spawns after manual start = %d, want 2 (manual bypasses backoff)", spawns)
	}

	// After the (reset, so 1s) backoff window elapses, a request retries.
	time.Sleep(1200 * time.Millisecond)
	resp, _ = supervisorGet(t, cfg, "/", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("post-backoff status = %d, want 503", resp.StatusCode)
	}
	if spawns := countSpawns(t, socket, "spawn-marker"); spawns != 3 {
		t.Errorf("spawns after backoff elapsed = %d, want 3 (request retries startup)", spawns)
	}
}
