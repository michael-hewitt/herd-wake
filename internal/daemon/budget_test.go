package daemon

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// budget fetches the daemon's running-server budget from status.
func (f *wildcardFixture) budget() control.BudgetStatus {
	f.t.Helper()
	status, err := f.client.Status(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	if status.Budget == nil {
		f.t.Fatal("status carries no budget")
	}
	return *status.Budget
}

// wildcardStatus fetches the fixture entry's status.
func (f *wildcardFixture) wildcardStatus() control.WildcardStatus {
	f.t.Helper()
	status, err := f.client.Status(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	if len(status.Wildcards) != 1 {
		f.t.Fatalf("status lists %d wildcard entries, want 1", len(status.Wildcards))
	}
	return status.Wildcards[0]
}

// slotHolders lists the projects that are starting or running, by name.
func slotHolders(projects []control.ProjectStatus) []string {
	var out []string
	for _, p := range projects {
		if p.State == StateRunning || p.State == StateStarting {
			out = append(out, p.Name)
		}
	}
	sort.Strings(out)
	return out
}

// assertSlotHolders checks exactly the named projects hold a slot.
func (f *wildcardFixture) assertSlotHolders(want ...string) {
	f.t.Helper()
	sort.Strings(want)
	if got := slotHolders(f.statusProjects()); strings.Join(got, ",") != strings.Join(want, ",") {
		f.t.Errorf("slot holders = %v, want %v", got, want)
	}
}

// staticProject renders a static testproc project for the fixture's
// projects: body.
func (f *wildcardFixture) staticProject(name, mode string, alwaysOn bool) string {
	sup, app := freePort(f.t), freePort(f.t)
	return fmt.Sprintf(`  %s:
    public_url: https://%s.test
    supervisor_port: %d
    application_port: %d
    working_directory: %s
    command: "%s=%s %s=%d %s"
    readiness_strategy: tcp
    startup_timeout_seconds: 10
    idle_timeout_seconds: 60
    shutdown_timeout_seconds: 5
    hold_max_wait_seconds: %d
    always_on: %t
`, name, name, sup, app, f.dir, testproc.EnvMode, mode, testproc.EnvPort, app, f.command, f.holdMaxWait, alwaysOn)
}

// TestBudgetEvictsLeastRecentlyActive: with max_running: 2 and three
// worktrees, waking the third stops the least-recently-active idle one
// first and serves the third; status reports 2/2 and which project is next
// in line; the evicted project cold-starts again on its next request,
// evicting the then-least-recent one.
func TestBudgetEvictsLeastRecentlyActive(t *testing.T) {
	f := newWildcardFixture(t)
	f.maxRunning = 2
	f.worktree("a")
	f.worktree("b")
	f.worktree("c")
	f.writeConfig("", "")
	f.start()

	_, _, aPid := f.get(f.host("a"), "/", nil)
	time.Sleep(20 * time.Millisecond) // b's last activity is strictly after a's
	_, _, bPid := f.get(f.host("b"), "/", nil)
	bSup := projectStatusByName(t, f.socket, "b").PID
	if b := f.budget(); b.Running != 2 || b.MaxRunning != 2 || b.NextEviction != "a" {
		t.Fatalf("budget with a and b running = %+v, want 2/2, next eviction a", b)
	}
	if w := f.wildcardStatus(); w.Running != 2 || w.MaxRunning != 0 || w.NextEviction != "a" {
		t.Errorf("wildcard status = %+v, want 2 running, no entry cap, next eviction a", w)
	}

	// The third wake-up evicts a (idle, least recently active) and serves c.
	code, body, cPid := f.get(f.host("c"), "/", nil)
	if code != http.StatusOK || cPid == 0 {
		t.Fatalf("c = %d; body:\n%s", code, body)
	}
	waitForProjectState(t, f.socket, "a", StateStopped, 15*time.Second)
	waitProcessGone(t, aPid)
	if b := projectStatusByName(t, f.socket, "b"); b.State != StateRunning || b.PID != bSup {
		t.Errorf("b after c's wake-up = %+v, want untouched (pid %d)", b, bSup)
	}
	if _, _, pid := f.get(f.host("b"), "/again", nil); pid != bPid {
		t.Errorf("b served by %d after c's wake-up, want %d", pid, bPid)
	}
	f.assertSlotHolders("b", "c")
	// b was just used, so c is now the least recently active.
	if b := f.budget(); b.Running != 2 || b.NextEviction != "c" {
		t.Errorf("budget = %+v, want 2/2 with c next", b)
	}
	if got := f.hasProject("a"); !got {
		t.Error("a should stay registered (stopped) after eviction")
	}

	// a's next visit cold-starts it normally, evicting c.
	code, body, aPid2 := f.get(f.host("a"), "/back", nil)
	if code != http.StatusOK || aPid2 == aPid || aPid2 == 0 {
		t.Fatalf("a after eviction = %d pid %d (old %d); body:\n%s", code, aPid2, aPid, body)
	}
	waitForProjectState(t, f.socket, "c", StateStopped, 15*time.Second)
	waitProcessGone(t, cPid)
	f.assertSlotHolders("a", "b")
	if n := countSpawnsFor(t, f.socket, "b", "testproc serving http"); n != 1 {
		t.Errorf("b spawned %d times, want 1 (never evicted)", n)
	}
}

// TestBudgetNothingEvictableHoldsThenRefuses: a project with an in-flight
// request, an open WebSocket, a lease, or always_on is never evicted; when
// those fill the budget a new wake-up is held (bounded by
// hold_max_wait_seconds) and then answered 503 "capacity" naming them and
// why, and project:start gets the same error. Once two of them free up, the
// wake-up evicts the least recently active of those.
func TestBudgetNothingEvictableHoldsThenRefuses(t *testing.T) {
	f := newWildcardFixture(t)
	f.maxRunning = 4
	f.holdMaxWait = 3
	f.worktree("a")
	f.worktree("b")
	f.setMode("b", testproc.ModeWS)
	f.worktree("c")
	f.worktree("d")
	f.writeConfig("", f.staticProject("keep", testproc.ModeListen, true))
	f.start()
	ctx := context.Background()
	waitForProjectState(t, f.socket, "keep", StateRunning, 10*time.Second)

	// a: a slow request in flight for the rest of the test.
	if code, body, _ := f.get(f.host("a"), "/", nil); code != http.StatusOK {
		t.Fatalf("a = %d; body:\n%s", code, body)
	}
	slowDone := make(chan int, 1)
	go func() {
		code, _, _ := f.get(f.host("a"), "/slow?sleep=12s", nil)
		slowDone <- code
	}()
	deadline := time.Now().Add(10 * time.Second)
	for projectStatusByName(t, f.socket, "a").InflightRequests == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a's slow request never showed up as in flight")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// b: an open WebSocket.
	ws, err := testproc.DialWS(fmt.Sprintf("127.0.0.1:%d", f.port), f.host("b"), "/ws", 30*time.Second)
	if err != nil {
		t.Fatalf("websocket to b: %v", err)
	}
	defer ws.Close() //nolint:errcheck // test cleanup
	// c: a lease.
	if code, body, _ := f.get(f.host("c"), "/", nil); code != http.StatusOK {
		t.Fatalf("c = %d; body:\n%s", code, body)
	}
	if _, err := f.client.LeaseProject(ctx, "c", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	f.assertSlotHolders("a", "b", "c", "keep")
	if b := f.budget(); b.Running != 4 || b.NextEviction != "" {
		t.Fatalf("budget = %+v, want 4/4 with nothing evictable", b)
	}

	// d: held for hold_max_wait minus the margin, then refused.
	started := time.Now()
	code, body, _ := f.get(f.host("d"), "/", nil)
	held := time.Since(started)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("d with a full budget = %d; body:\n%s", code, body)
	}
	for _, want := range []string{
		"capacity",
		"4 of 4 are taken (max_running)",
		`"keep" is always_on`,
		`"a" has 1 request(s) in flight`,
		`"b" has 1 open WebSocket(s)`,
		`"c" is leased until`,
		"project:stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("capacity 503 missing %q:\n%s", want, body)
		}
	}
	if held < 1500*time.Millisecond || held > 6*time.Second {
		t.Errorf("d was held %s, want about 2s (hold_max_wait 3s minus the margin)", held)
	}
	f.assertSlotHolders("a", "b", "c", "keep")
	if n := countSpawnsFor(t, f.socket, "d", "testproc serving http"); n != 0 {
		t.Errorf("d spawned %d times while refused, want 0", n)
	}
	// Browsers get the HTML page with the title.
	code, body, _ = f.get(f.host("d"), "/", http.Header{"Accept": []string{"text/html"}})
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "<title>herd-wake: no running-server slot") {
		t.Errorf("HTML capacity 503 = %d; body:\n%s", code, body)
	}

	// Manual project:start gets the same rule and a clear error.
	started = time.Now()
	_, err = f.client.StartProject(ctx, "d")
	held = time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "no running-server slot") || !strings.Contains(err.Error(), `"keep" is always_on`) {
		t.Errorf("StartProject(d) = %v, want a capacity error naming the blockers", err)
	}
	if held < 2500*time.Millisecond || held > 8*time.Second {
		t.Errorf("StartProject(d) waited %s, want about 3s (hold_max_wait)", held)
	}

	// Free b (socket closed) and c (lease released): the next wake-up evicts
	// c, whose last request predates b's socket closing; a is still busy.
	if err := ws.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.ReleaseProjectLease(ctx, "c"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for f.budget().NextEviction != "c" {
		if time.Now().After(deadline) {
			t.Fatalf("next eviction = %q, want c once b's socket closed and c's lease is released", f.budget().NextEviction)
		}
		time.Sleep(10 * time.Millisecond)
	}
	code, body, _ = f.get(f.host("d"), "/", nil)
	if code != http.StatusOK {
		t.Fatalf("d after freeing b and c = %d; body:\n%s", code, body)
	}
	waitForProjectState(t, f.socket, "c", StateStopped, 15*time.Second)
	f.assertSlotHolders("a", "b", "d", "keep")

	// a's slow request completes against its original process.
	select {
	case code := <-slowDone:
		if code != http.StatusOK {
			t.Errorf("slow request = %d, want 200", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("slow request never completed")
	}
	if n := countSpawnsFor(t, f.socket, "a", "testproc serving http"); n != 1 {
		t.Errorf("a spawned %d times, want 1 (never evicted while busy)", n)
	}
}

// TestBudgetConcurrentWakeupsNeverExceedCap: simultaneous wake-ups of many
// stopped projects at the limit are admitted one at a time — every request
// is eventually served, and at no point do more than max_running projects
// hold a slot. The count is sampled under the budget lock, which is the
// only consistent view (the control API reads each project's state in
// turn, so it can see a victim before its stop and the newcomer after its
// start); a start that bypassed the lock would show up as an excess.
func TestBudgetConcurrentWakeupsNeverExceedCap(t *testing.T) {
	f := newWildcardFixture(t)
	f.maxRunning = 2
	labels := []string{"a", "b", "c", "d", "e", "f"}
	for _, label := range labels {
		f.worktree(label)
	}
	f.writeConfig("", "")
	f.start()
	for _, label := range labels[:2] {
		if code, body, _ := f.get(f.host(label), "/", nil); code != http.StatusOK {
			t.Fatalf("%s = %d; body:\n%s", label, code, body)
		}
	}

	// Sample the slot count continuously.
	var over, peak, samples atomic.Int64
	sampling := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-sampling:
				return
			default:
			}
			f.d.budgetMu.Lock()
			n, _ := countRunning(f.d.sortedStates(), nil)
			f.d.budgetMu.Unlock()
			samples.Add(1)
			if int64(n) > peak.Load() {
				peak.Store(int64(n))
			}
			if n > 2 {
				over.Add(1)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// Two waves of simultaneous wake-ups of the four stopped worktrees.
	for wave := range 2 {
		var wg sync.WaitGroup
		for _, label := range labels[2:] {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, body, _ := f.get(f.host(label), fmt.Sprintf("/wave/%d", wave), nil)
				if code != http.StatusOK {
					t.Errorf("wave %d: %s = %d; body:\n%s", wave, label, code, body)
				}
			}()
		}
		wg.Wait()
	}
	close(sampling)
	<-sampled

	if n := over.Load(); n != 0 {
		t.Errorf("observed more than 2 slot holders in %d of %d samples (peak %d)", n, samples.Load(), peak.Load())
	}
	if peak.Load() != 2 {
		t.Errorf("peak slot holders = %d over %d samples, want the cap of 2 reached", peak.Load(), samples.Load())
	}
	if holders := slotHolders(f.statusProjects()); len(holders) > 2 {
		t.Errorf("slot holders at the end = %v, want at most 2", holders)
	}
}

// TestBudgetPerEntryCap: an entry's max_running is enforced on its own
// worktrees even when the top-level budget has room (or is unset), and a
// static project is untouched by it.
func TestBudgetPerEntryCap(t *testing.T) {
	f := newWildcardFixture(t)
	f.worktree("a")
	f.worktree("b")
	f.writeConfig("    max_running: 1\n", f.staticProject("solo", testproc.ModeListen, false))
	f.start()
	ctx := context.Background()

	if _, err := f.client.StartProject(ctx, "solo"); err != nil {
		t.Fatalf("StartProject(solo): %v", err)
	}
	_, _, aPid := f.get(f.host("a"), "/", nil)
	if w := f.wildcardStatus(); w.Running != 1 || w.MaxRunning != 1 || w.NextEviction != "a" {
		t.Errorf("wildcard status = %+v, want running 1/1 with a next", w)
	}
	if b := f.budget(); b.Running != 2 || b.MaxRunning != 0 {
		t.Errorf("budget = %+v, want 2 running, no top-level cap", b)
	}

	// b evicts a under the entry cap although nothing caps the total.
	code, body, _ := f.get(f.host("b"), "/", nil)
	if code != http.StatusOK {
		t.Fatalf("b = %d; body:\n%s", code, body)
	}
	waitForProjectState(t, f.socket, "a", StateStopped, 15*time.Second)
	waitProcessGone(t, aPid)
	f.assertSlotHolders("b", "solo")
	if w := f.wildcardStatus(); w.Running != 1 || w.NextEviction != "b" {
		t.Errorf("wildcard status = %+v, want running 1/1 with b next", w)
	}

	// A busy b fills the entry cap: a is refused with the entry named.
	if _, err := f.client.LeaseProject(ctx, "b", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	code, body, _ = f.get(f.host("a"), "/", nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `1 of 1 are taken (discovery "webapp": max_running)`) ||
		!strings.Contains(body, `"b" is leased until`) {
		t.Errorf("a under a full entry cap = %d; body:\n%s", code, body)
	}
	f.assertSlotHolders("b", "solo")
}

// TestBudgetReloadAppliesCapLive: changing max_running (top-level or per
// entry) and reloading takes effect without rebuilding the entry or
// stopping anything; lowering it below the current count stops nothing
// until the next wake-up, which then evicts down to the cap; raising or
// removing it lets projects start freely again.
func TestBudgetReloadAppliesCapLive(t *testing.T) {
	f := newWildcardFixture(t)
	f.worktree("a")
	f.worktree("b")
	f.worktree("c")
	f.worktree("d")
	f.writeConfig("", "")
	f.start()

	pids := map[string]int{}
	for _, label := range []string{"a", "b", "c"} {
		if code, body, _ := f.get(f.host(label), "/", nil); code != http.StatusOK {
			t.Fatalf("%s = %d; body:\n%s", label, code, body)
		}
		pids[label] = projectStatusByName(t, f.socket, label).PID
		time.Sleep(20 * time.Millisecond)
	}
	if b := f.budget(); b.Running != 3 || b.MaxRunning != 0 {
		t.Fatalf("budget unlimited = %+v", b)
	}

	// Lower the cap below the current count: nothing stops.
	f.maxRunning = 2
	f.writeConfig("", "")
	resp := f.reload()
	if names(resp.Wildcards.Unchanged) != "webapp" || len(resp.Removed) != 0 || len(resp.Errors) != 0 {
		t.Fatalf("reload lowering max_running = %+v (wildcards %+v), want the entry unchanged", resp, resp.Wildcards)
	}
	for label, pid := range pids {
		if p := projectStatusByName(t, f.socket, label); p.State != StateRunning || p.PID != pid {
			t.Errorf("%s after lowering the cap = %+v, want still running with pid %d", label, p, pid)
		}
	}
	if b := f.budget(); b.Running != 3 || b.MaxRunning != 2 || b.NextEviction != "a" {
		t.Errorf("budget after lowering = %+v, want 3/2 with a next", b)
	}

	// The next wake-up evicts down to the cap: a and b go, c stays.
	if code, body, _ := f.get(f.host("d"), "/", nil); code != http.StatusOK {
		t.Fatalf("d = %d; body:\n%s", code, body)
	}
	waitForProjectState(t, f.socket, "a", StateStopped, 15*time.Second)
	waitForProjectState(t, f.socket, "b", StateStopped, 15*time.Second)
	f.assertSlotHolders("c", "d")
	if p := projectStatusByName(t, f.socket, "c"); p.PID != pids["c"] {
		t.Errorf("c = %+v, want untouched (pid %d)", p, pids["c"])
	}

	// A per-entry cap arrives by reload too, without touching the entry.
	f.maxRunning = 0
	f.writeConfig("    max_running: 1\n", "")
	resp = f.reload()
	if names(resp.Wildcards.Unchanged) != "webapp" || len(resp.Removed) != 0 {
		t.Fatalf("reload adding the entry cap = %+v (wildcards %+v), want the entry unchanged", resp, resp.Wildcards)
	}
	f.assertSlotHolders("c", "d")
	if w := f.wildcardStatus(); w.Running != 2 || w.MaxRunning != 1 {
		t.Errorf("wildcard status after adding the entry cap = %+v, want 2/1", w)
	}
	if code, body, _ := f.get(f.host("a"), "/", nil); code != http.StatusOK {
		t.Fatalf("a under the entry cap = %d; body:\n%s", code, body)
	}
	waitForProjectState(t, f.socket, "c", StateStopped, 15*time.Second)
	waitForProjectState(t, f.socket, "d", StateStopped, 15*time.Second)
	f.assertSlotHolders("a")

	// Removing every cap: starts no longer evict.
	f.writeConfig("", "")
	if resp := f.reload(); names(resp.Wildcards.Unchanged) != "webapp" {
		t.Fatalf("reload removing the caps = %+v", resp.Wildcards)
	}
	for _, label := range []string{"b", "c"} {
		if code, body, _ := f.get(f.host(label), "/", nil); code != http.StatusOK {
			t.Fatalf("%s uncapped = %d; body:\n%s", label, code, body)
		}
	}
	f.assertSlotHolders("a", "b", "c")
	if b := f.budget(); b.Running != 3 || b.MaxRunning != 0 {
		t.Errorf("budget uncapped = %+v", b)
	}
}

// TestBudgetAlwaysOnStartsWithinCap: always_on projects take their slots
// at daemon start, before any request, and count against the budget.
func TestBudgetAlwaysOnStartsWithinCap(t *testing.T) {
	f := newWildcardFixture(t)
	f.maxRunning = 2
	f.worktree("a")
	f.writeConfig("", f.staticProject("keep", testproc.ModeListen, true))
	f.start()

	waitForProjectState(t, f.socket, "keep", StateRunning, 10*time.Second)
	if b := f.budget(); b.Running != 1 || b.NextEviction != "" {
		t.Errorf("budget with only always_on = %+v, want 1/2 and nothing evictable", b)
	}
	if code, body, _ := f.get(f.host("a"), "/", nil); code != http.StatusOK {
		t.Fatalf("a = %d; body:\n%s", code, body)
	}
	if b := f.budget(); b.Running != 2 || b.NextEviction != "a" {
		t.Errorf("budget = %+v, want 2/2 with a next (never keep)", b)
	}
}
