package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// idleProject builds a one-project HTTP-echo config with a second-scale idle
// timeout. idle_timeout_minutes is deliberately left at a large value so the
// tests also prove idle_timeout_seconds takes precedence.
func idleProject(t *testing.T, idleSeconds int) *config.Config {
	t.Helper()
	cfg := httpProject(t)
	p := cfg.Projects["dashboard"]
	p.IdleTimeoutMinutes = 60
	p.IdleTimeoutSeconds = idleSeconds
	return cfg
}

// projectStatusByName fetches one project's status over the control socket.
func projectStatusByName(t *testing.T, socket, name string) control.ProjectStatus {
	t.Helper()
	status, err := control.NewClient(socket).Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, p := range status.Projects {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("project %q not in status %+v", name, status.Projects)
	return control.ProjectStatus{}
}

// waitForProjectState polls until the named project reaches the wanted
// lifecycle state.
func waitForProjectState(t *testing.T, socket, name, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := projectStatusByName(t, socket, name).State
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("project %q state = %q after %s, want %q", name, got, timeout, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// echoPid extracts the serving process's pid from a testproc HTTP echo body.
func echoPid(t *testing.T, body string) int {
	t.Helper()
	m := pidPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("echo body missing pid marker: %q", body)
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse pid from %q: %v", body, err)
	}
	return pid
}

// TestIdleStopThenColdStart is the core idle acceptance path: a running
// project with no traffic stops gracefully after its (second-scale) idle
// timeout, its process group is really gone, and the next request
// cold-starts a fresh process.
func TestIdleStopThenColdStart(t *testing.T) {
	cfg := idleProject(t, 1)
	socket, _, _ := startDaemon(t, cfg)

	resp, body := supervisorGet(t, cfg, "/", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("cold-start status = %d, want 200; body:\n%s", resp.StatusCode, body)
	}
	firstPid := echoPid(t, body)

	// No more traffic: the project must stop on its own after ~1s.
	waitForProjectState(t, socket, "dashboard", StateStopped, 10*time.Second)
	waitProcessGone(t, firstPid)

	// The next request cold-starts a fresh process.
	resp, body = supervisorGet(t, cfg, "/", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("post-idle-stop status = %d, want 200; body:\n%s", resp.StatusCode, body)
	}
	if again := echoPid(t, body); again == firstPid {
		t.Errorf("post-idle-stop pid = %d, want a fresh process (old pid %d)", again, firstPid)
	}
	waitForProjectState(t, socket, "dashboard", StateRunning, 5*time.Second)
}

// TestRequestsResetIdleCountdown: traffic inside the idle window keeps the
// project alive — the same process serves throughout — and only once the
// traffic stops does the idle stop happen.
func TestRequestsResetIdleCountdown(t *testing.T) {
	cfg := idleProject(t, 2)
	socket, _, _ := startDaemon(t, cfg)

	_, body := supervisorGet(t, cfg, "/", nil)
	pid := echoPid(t, body)

	// Requests every ~700ms for ~2.8s: each one resets the 2s countdown, so
	// the project must never stop (and never respawn) during the traffic.
	for i := range 4 {
		time.Sleep(700 * time.Millisecond)
		resp, body := supervisorGet(t, cfg, fmt.Sprintf("/tick/%d", i), nil)
		if resp.StatusCode != 200 {
			t.Fatalf("request %d during traffic: status = %d; body:\n%s", i, resp.StatusCode, body)
		}
		if got := echoPid(t, body); got != pid {
			t.Fatalf("request %d served by pid %d, want the original process %d (no idle stop during traffic)", i, got, pid)
		}
	}
	if spawns := countSpawns(t, socket, "testproc serving http"); spawns != 1 {
		t.Errorf("spawns during traffic = %d, want 1 (idle stop must not fire between requests)", spawns)
	}

	// Traffic over: the idle stop follows.
	waitForProjectState(t, socket, "dashboard", StateStopped, 10*time.Second)
}

// TestInFlightRequestParksIdleStop: a single long-running request far past
// the idle timeout parks the countdown entirely; the full idle window only
// starts once the request completes.
func TestInFlightRequestParksIdleStop(t *testing.T) {
	cfg := idleProject(t, 1)
	socket, _, _ := startDaemon(t, cfg)

	_, body := supervisorGet(t, cfg, "/", nil)
	pid := echoPid(t, body)

	// A request that stays in flight for 3s — three times the idle timeout.
	type slowResult struct {
		status int
		body   string
	}
	slowDone := make(chan slowResult, 1)
	go func() {
		resp, body := supervisorGet(t, cfg, "/slow?sleep=3s", nil)
		slowDone <- slowResult{status: resp.StatusCode, body: body}
	}()

	// Well past the timeout, with the request still in flight, the project
	// must still be running and status must show the in-flight hold.
	time.Sleep(2 * time.Second)
	p := projectStatusByName(t, socket, "dashboard")
	if p.State != StateRunning {
		t.Fatalf("state with a request in flight = %q, want %q", p.State, StateRunning)
	}
	if p.InflightRequests != 1 {
		t.Errorf("InflightRequests = %d, want 1", p.InflightRequests)
	}
	if !p.IdleStopAt.IsZero() {
		t.Errorf("IdleStopAt = %v, want zero while the countdown is parked", p.IdleStopAt)
	}

	// The request completes against the original process — it was never
	// stopped or restarted underneath it.
	select {
	case res := <-slowDone:
		if res.status != 200 {
			t.Fatalf("slow request status = %d, want 200; body:\n%s", res.status, res.body)
		}
		if got := echoPid(t, res.body); got != pid {
			t.Errorf("slow request served by pid %d, want %d", got, pid)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("slow request never completed")
	}
	if spawns := countSpawns(t, socket, "testproc serving http"); spawns != 1 {
		t.Errorf("spawns = %d, want 1 (no restart during the in-flight request)", spawns)
	}

	// Completion starts a fresh idle window; the stop follows it.
	waitForProjectState(t, socket, "dashboard", StateStopped, 10*time.Second)
}

// TestAlwaysOnStartsWithDaemonAndNeverIdles: an always_on project starts as
// the daemon comes up (no request needed), is never idle-stopped, and can
// still be restarted manually.
func TestAlwaysOnStartsWithDaemonAndNeverIdles(t *testing.T) {
	cfg := idleProject(t, 1)
	cfg.Projects["dashboard"].AlwaysOn = true
	socket, _, _ := startDaemon(t, cfg)

	// Started by the daemon itself.
	waitForProjectState(t, socket, "dashboard", StateRunning, 10*time.Second)
	first := projectStatusByName(t, socket, "dashboard")
	if !first.AlwaysOn {
		t.Error("status should mark the project always_on")
	}
	if !first.IdleStopAt.IsZero() {
		t.Errorf("IdleStopAt = %v, want zero for an always_on project", first.IdleStopAt)
	}

	// Far past the idle timeout with zero traffic: still the same process.
	time.Sleep(2500 * time.Millisecond)
	after := projectStatusByName(t, socket, "dashboard")
	if after.State != StateRunning || after.PID != first.PID {
		t.Errorf("after idle window: state=%q pid=%d, want running with pid %d (never idle-stopped)",
			after.State, after.PID, first.PID)
	}

	// Manual restart still works.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	restarted, err := control.NewClient(socket).RestartProject(ctx, "dashboard")
	if err != nil {
		t.Fatalf("RestartProject: %v", err)
	}
	if restarted.State != StateRunning || restarted.PID == first.PID {
		t.Errorf("restart status = %+v, want running with a new pid (old %d)", restarted, first.PID)
	}
}

// TestAlwaysOnStartFailureDoesNotBlockDaemon: a crashing always_on project
// must not keep the daemon from starting — it is marked failed and the
// daemon keeps answering.
func TestAlwaysOnStartFailureDoesNotBlockDaemon(t *testing.T) {
	cfg := crashProject(t, "echo always-on-boom >&2; exit 5")
	cfg.Projects["dashboard"].AlwaysOn = true
	socket, _, _ := startDaemon(t, cfg)

	waitForProjectState(t, socket, "dashboard", StateFailed, 10*time.Second)
	p := projectStatusByName(t, socket, "dashboard")
	if p.LastError == "" {
		t.Error("failed always_on project should carry LastError")
	}
	// The daemon itself is healthy: the control socket keeps answering.
	if _, err := control.NewClient(socket).Status(context.Background()); err != nil {
		t.Errorf("daemon stopped answering after always_on start failure: %v", err)
	}
}

// TestLeaseParksIdleStopUntilExpiry: an activity lease keeps an otherwise
// idle project running until the lease expires, after which the idle stop
// happens on its own.
func TestLeaseParksIdleStopUntilExpiry(t *testing.T) {
	cfg := idleProject(t, 1)
	socket, _, _ := startDaemon(t, cfg)
	client := control.NewClient(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Lease first (a lease may be taken while stopped), then start with no
	// traffic at all.
	leased, err := client.LeaseProject(ctx, "dashboard", 3*time.Second)
	if err != nil {
		t.Fatalf("LeaseProject: %v", err)
	}
	if leased.LeaseUntil.IsZero() {
		t.Fatal("lease response should carry LeaseUntil")
	}
	if _, err := client.StartProject(ctx, "dashboard"); err != nil {
		t.Fatalf("StartProject: %v", err)
	}

	// Twice the idle timeout later the lease is still parking the stop.
	time.Sleep(2 * time.Second)
	p := projectStatusByName(t, socket, "dashboard")
	if p.State != StateRunning {
		t.Fatalf("state during lease = %q, want %q", p.State, StateRunning)
	}
	if p.LeaseUntil.IsZero() || !p.IdleStopAt.IsZero() {
		t.Errorf("status during lease = lease_until %v, idle_stop_at %v; want an active lease and no scheduled stop",
			p.LeaseUntil, p.IdleStopAt)
	}

	// Lease expiry: normal idle rules resume and the project stops.
	waitForProjectState(t, socket, "dashboard", StateStopped, 10*time.Second)
}

// TestLeaseReleaseAllowsIdleStop: releasing a long lease early puts the
// project back under normal idle rules.
func TestLeaseReleaseAllowsIdleStop(t *testing.T) {
	cfg := idleProject(t, 1)
	socket, _, _ := startDaemon(t, cfg)
	client := control.NewClient(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.LeaseProject(ctx, "dashboard", 10*time.Minute); err != nil {
		t.Fatalf("LeaseProject: %v", err)
	}
	if _, err := client.StartProject(ctx, "dashboard"); err != nil {
		t.Fatalf("StartProject: %v", err)
	}

	time.Sleep(1500 * time.Millisecond)
	if p := projectStatusByName(t, socket, "dashboard"); p.State != StateRunning {
		t.Fatalf("state during 10m lease = %q, want %q", p.State, StateRunning)
	}

	released, err := client.ReleaseProjectLease(ctx, "dashboard")
	if err != nil {
		t.Fatalf("ReleaseProjectLease: %v", err)
	}
	if !released.LeaseUntil.IsZero() {
		t.Errorf("LeaseUntil after release = %v, want zero", released.LeaseUntil)
	}
	waitForProjectState(t, socket, "dashboard", StateStopped, 10*time.Second)
}

// TestStatusReportsActivityAndScheduledIdleStop: status carries the
// last-request time and the scheduled idle-stop time for a running project.
func TestStatusReportsActivityAndScheduledIdleStop(t *testing.T) {
	cfg := idleProject(t, 60) // long enough that nothing stops mid-test
	socket, _, _ := startDaemon(t, cfg)

	before := time.Now()
	if resp, body := supervisorGet(t, cfg, "/", nil); resp.StatusCode != 200 {
		t.Fatalf("request status = %d; body:\n%s", resp.StatusCode, body)
	}

	p := projectStatusByName(t, socket, "dashboard")
	if p.State != StateRunning {
		t.Fatalf("state = %q, want %q", p.State, StateRunning)
	}
	if p.LastActivityAt.IsZero() || p.LastActivityAt.Before(before.Add(-time.Second)) {
		t.Errorf("LastActivityAt = %v, want the just-completed request (>= %v)", p.LastActivityAt, before)
	}
	if p.IdleStopAt.IsZero() {
		t.Fatal("IdleStopAt should be scheduled for a running idle-managed project")
	}
	if got, want := p.IdleStopAt.Sub(p.LastActivityAt), 60*time.Second; got != want {
		t.Errorf("IdleStopAt - LastActivityAt = %v, want %v (the idle timeout)", got, want)
	}
	if p.InflightRequests != 0 {
		t.Errorf("InflightRequests = %d, want 0", p.InflightRequests)
	}
}

// twoIdleProjects builds a config with two HTTP-echo projects: "alpha" with
// a 1s idle timeout and "beta" with a much longer one.
func twoIdleProjects(t *testing.T) *config.Config {
	t.Helper()
	command, err := testproc.Command()
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, idleSeconds int) *config.Project {
		applicationPort := freePort(t)
		return &config.Project{
			Name:                   name,
			PublicURL:              "https://" + name + ".test",
			SupervisorPort:         freePort(t),
			ApplicationPort:        applicationPort,
			ListenHost:             "127.0.0.1",
			WorkingDirectory:       t.TempDir(),
			Command:                command,
			ReadinessStrategy:      config.ReadinessTCP,
			StartupTimeoutSeconds:  10,
			IdleTimeoutSeconds:     idleSeconds,
			IdleTimeoutMinutes:     60,
			ShutdownSignal:         "SIGTERM",
			ShutdownTimeoutSeconds: 5,
			LogRetentionDays:       7,
			HoldMaxWaitSeconds:     15,
			HoldMaxRequests:        100,
			Env: map[string]string{
				testproc.EnvMode: testproc.ModeHTTP,
				testproc.EnvPort: strconv.Itoa(applicationPort),
			},
		}
	}
	return &config.Config{Projects: map[string]*config.Project{
		"alpha": mk("alpha", 1),
		"beta":  mk("beta", 300),
	}}
}

// TestIdleStopIsolatedBetweenProjects: idle-stopping one project leaves the
// other project's process untouched and still serving.
func TestIdleStopIsolatedBetweenProjects(t *testing.T) {
	cfg := twoIdleProjects(t)
	socket, _, _ := startDaemon(t, cfg)

	get := func(name, path string) (int, string) {
		t.Helper()
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Projects[name].SupervisorPort, path))
		if err != nil {
			t.Fatalf("request to %s: %v", name, err)
		}
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read response body from %s: %v", name, err)
		}
		return resp.StatusCode, string(body)
	}

	code, body := get("alpha", "/")
	if code != 200 {
		t.Fatalf("alpha cold start = %d; body:\n%s", code, body)
	}
	alphaPid := echoPid(t, body)
	code, body = get("beta", "/")
	if code != 200 {
		t.Fatalf("beta cold start = %d; body:\n%s", code, body)
	}
	betaPid := echoPid(t, body)

	// Alpha idles out; beta must be completely unaffected.
	waitForProjectState(t, socket, "alpha", StateStopped, 10*time.Second)
	waitProcessGone(t, alphaPid)

	beta := projectStatusByName(t, socket, "beta")
	if beta.State != StateRunning || beta.PID != betaPid {
		t.Fatalf("beta after alpha's idle stop = state %q pid %d, want running with pid %d", beta.State, beta.PID, betaPid)
	}
	code, body = get("beta", "/still-alive")
	if code != 200 || echoPid(t, body) != betaPid {
		t.Errorf("beta request after alpha's idle stop = %d from pid %s, want 200 from pid %d",
			code, pidPattern.FindString(body), betaPid)
	}
}

// TestRequestDuringIdleStopIsHeldThenColdStarts: the process ignores SIGTERM,
// so its idle stop reliably spends the full shutdown timeout in "stopping" —
// a request fired into that window must not be lost or answered by the dying
// process: it waits for the stop to finish and is served by a fresh process.
func TestRequestDuringIdleStopIsHeldThenColdStarts(t *testing.T) {
	cfg := idleProject(t, 1)
	p := cfg.Projects["dashboard"]
	p.Env[testproc.EnvMode] = testproc.ModeHTTPStubborn
	p.ShutdownTimeoutSeconds = 2
	socket, _, _ := startDaemon(t, cfg)

	_, body := supervisorGet(t, cfg, "/", nil)
	firstPid := echoPid(t, body)

	// Wait for the idle stop to begin (stubborn process: the stopping state
	// lasts the full 2s shutdown timeout).
	waitForProjectState(t, socket, "dashboard", StateStopping, 10*time.Second)

	// A request landing mid-stop: held, then served by the restarted
	// project — never by the dying process, and never dropped.
	resp, body := supervisorGet(t, cfg, "/during-idle-stop", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("request during idle stop = %d, want 200; body:\n%s", resp.StatusCode, body)
	}
	if got := echoPid(t, body); got == firstPid {
		t.Errorf("request during idle stop answered by the dying process %d, want a fresh one", got)
	}
	waitForProjectState(t, socket, "dashboard", StateRunning, 5*time.Second)
}
