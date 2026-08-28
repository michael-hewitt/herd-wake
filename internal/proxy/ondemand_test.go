package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/idle"
	"github.com/michael-hewitt/herd-wake/internal/process"
)

// fakeUpstream implements Upstream without any real process, so hold
// behavior can be tested deterministically.
type fakeUpstream struct {
	state       atomic.Value // string
	ensureCalls atomic.Int64
	// ensure produces the EnsureStartedOnDemand outcome channel.
	ensure func() <-chan error

	mu   sync.Mutex
	snap process.Snapshot
	logs []string
}

func newFakeUpstream(state string) *fakeUpstream {
	f := &fakeUpstream{}
	f.state.Store(state)
	f.snap = process.Snapshot{State: state}
	return f
}

func (f *fakeUpstream) State() string { return f.state.Load().(string) }

func (f *fakeUpstream) EnsureStartedOnDemand() <-chan error {
	f.ensureCalls.Add(1)
	return f.ensure()
}

func (f *fakeUpstream) Snapshot() process.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeUpstream) Logs(int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs
}

// onDemandHandler builds the handler under test — with a real activity
// tracker — giving direct access to the onDemand internals for sub-second
// wait bounds.
func onDemandHandler(t *testing.T, p *config.Project, up Upstream) *onDemand {
	t.Helper()
	h, ok := NewOnDemand(p, up, idle.NewTracker(), discardLogger()).(*onDemand)
	if !ok {
		t.Fatal("NewOnDemand did not return *onDemand")
	}
	return h
}

// tracker returns the handler's activity tracker.
func tracker(t *testing.T, h *onDemand) *idle.Tracker {
	t.Helper()
	tr, ok := h.activity.(*idle.Tracker)
	if !ok {
		t.Fatalf("handler activity = %T, want *idle.Tracker", h.activity)
	}
	return tr
}

func TestOnDemandHotPathForwardsWithoutStartCalls(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "upstream saw %s", r.URL.Path)
	}))
	defer upstream.Close()

	fake := newFakeUpstream(process.StateRunning)
	front := httptest.NewServer(onDemandHandler(t, testProject(serverPort(t, upstream)), fake))
	defer front.Close()

	resp, err := front.Client().Get(front.URL + "/hot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || string(body) != "upstream saw /hot" {
		t.Errorf("response = %d %q, want 200 %q", resp.StatusCode, body, "upstream saw /hot")
	}
	if calls := fake.ensureCalls.Load(); calls != 0 {
		t.Errorf("EnsureStartedOnDemand called %d times on the hot path, want 0", calls)
	}
}

// TestOnDemandHotPathDoesNotSerialize proves lifecycle handling adds no
// contention for a running project: while one request is blocked in the
// upstream, another request completes.
func TestOnDemandHotPathDoesNotSerialize(t *testing.T) {
	slowStarted := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(slowStarted)
			<-release
		}
		fmt.Fprint(w, "done")
	}))
	defer upstream.Close()

	fake := newFakeUpstream(process.StateRunning)
	front := httptest.NewServer(onDemandHandler(t, testProject(serverPort(t, upstream)), fake))
	defer front.Close()

	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		resp, err := front.Client().Get(front.URL + "/slow")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-slowStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("slow request never reached the upstream")
	}

	// The concurrent request must complete while /slow is still held.
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(front.URL + "/fast")
	if err != nil {
		t.Fatalf("concurrent request blocked behind a slow one: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("concurrent request status = %d, want 200", resp.StatusCode)
	}
	close(release)
	<-slowDone
}

// recordingReader records when its content is first read.
type recordingReader struct {
	r         io.Reader
	firstRead atomic.Int64 // unix nanos of the first Read; 0 = never
}

func (rr *recordingReader) Read(p []byte) (int, error) {
	rr.firstRead.CompareAndSwap(0, time.Now().UnixNano())
	return rr.r.Read(p)
}

// TestOnDemandHoldsRequestWithoutReadingBody is the core hold behavior: a
// request arriving while the project starts is held untouched — its body is
// not read until the upstream is ready — then forwarded intact.
func TestOnDemandHoldsRequestWithoutReadingBody(t *testing.T) {
	var gotBody atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody.Store(string(body))
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()

	fake := newFakeUpstream(process.StateStarting)
	var releasedAt atomic.Int64
	fake.ensure = func() <-chan error {
		ch := make(chan error, 1)
		go func() {
			time.Sleep(150 * time.Millisecond)
			releasedAt.Store(time.Now().UnixNano())
			fake.state.Store(process.StateRunning)
			ch <- nil
		}()
		return ch
	}
	h := onDemandHandler(t, testProject(serverPort(t, upstream)), fake)

	body := &recordingReader{r: strings.NewReader("held body payload")}
	req := httptest.NewRequest(http.MethodPost, "http://dashboard.test/submit", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	if got := gotBody.Load(); got != "held body payload" {
		t.Errorf("upstream body = %q, want %q", got, "held body payload")
	}
	first := body.firstRead.Load()
	if first == 0 {
		t.Fatal("request body was never read")
	}
	if released := releasedAt.Load(); first < released {
		t.Errorf("request body was read %s before the upstream became ready; held bodies must not be consumed",
			time.Duration(released-first))
	}
	if calls := fake.ensureCalls.Load(); calls != 1 {
		t.Errorf("EnsureStartedOnDemand called %d times, want 1", calls)
	}
}

func TestOnDemandHoldCountLimitReturns503(t *testing.T) {
	fake := newFakeUpstream(process.StateStarting)
	never := make(chan error) // startup never resolves
	fake.ensure = func() <-chan error { return never }

	p := testProject(0)
	p.HoldMaxRequests = 2
	p.HoldMaxWaitSeconds = 60
	h := onDemandHandler(t, p, fake)

	// Fill both hold slots with requests that wait until cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "http://dashboard.test/", nil).WithContext(ctx)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.held.Load() != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("held = %d, want 2", h.held.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The third request is over the limit: refused immediately.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://dashboard.test/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hold_max_requests") {
		t.Errorf("503 body should explain the hold limit; got:\n%s", rec.Body.String())
	}

	cancel()
	wg.Wait()

	// With slots free again, the limit no longer trips (the request waits;
	// cancel it via context to end the test).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "http://dashboard.test/", nil).WithContext(ctx2))
	if strings.Contains(rec2.Body.String(), "hold_max_requests") {
		t.Errorf("hold slots were not released; got:\n%s", rec2.Body.String())
	}
}

func TestOnDemandMaxWaitReturns503(t *testing.T) {
	fake := newFakeUpstream(process.StateStarting)
	never := make(chan error)
	fake.ensure = func() <-chan error { return never }
	fake.mu.Lock()
	fake.snap = process.Snapshot{State: process.StateStarting}
	fake.mu.Unlock()

	h := onDemandHandler(t, testProject(0), fake)
	h.maxWait = 100 * time.Millisecond // sub-second override for the test

	begin := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://dashboard.test/", nil))

	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Errorf("wait-bounded request took %s", elapsed)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hold_max_wait_seconds") || !strings.Contains(body, "starting") {
		t.Errorf("503 body should explain the wait limit and state; got:\n%s", body)
	}
}

// failingFake returns a fake whose startup resolves immediately with err and
// whose snapshot/logs mimic a crashed project.
func failingFake(err error) *fakeUpstream {
	fake := newFakeUpstream(process.StateStopped)
	fake.ensure = func() <-chan error {
		ch := make(chan error, 1)
		ch <- err
		return ch
	}
	fake.mu.Lock()
	fake.snap = process.Snapshot{
		State:     process.StateFailed,
		LastExit:  "exit status 3",
		LastError: "process exited during startup",
	}
	fake.logs = []string{"boom line one", "<script>alert('boom')</script>"}
	fake.mu.Unlock()
	return fake
}

func TestOnDemandFailureDiagnosticPlainText(t *testing.T) {
	fake := failingFake(errors.New(`project "dashboard": process exited during startup (exit status 3)`))
	h := onDemandHandler(t, testProject(0), fake)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://dashboard.test/", nil)
	req.Header.Set("Accept", "*/*") // curl's default: not a browser
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`"dashboard"`, "failed", "exit status 3", "boom line one"} {
		if !strings.Contains(body, want) {
			t.Errorf("plain diagnostic missing %q; got:\n%s", want, body)
		}
	}
}

func TestOnDemandFailureDiagnosticHTMLEscaped(t *testing.T) {
	fake := failingFake(errors.New(`project "dashboard": process exited during startup (exit status 3)`))
	h := onDemandHandler(t, testProject(0), fake)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://dashboard.test/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"<!DOCTYPE html", "dashboard", "exit status 3", "boom line one"} {
		if !strings.Contains(body, want) {
			t.Errorf("HTML diagnostic missing %q; got:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("HTML diagnostic must escape process output")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("HTML diagnostic should contain the escaped log line; got:\n%s", body)
	}
}

// TestOnDemandTracksActivity: every request is bracketed by
// RequestBegan/RequestEnded — the in-flight count is visible while the
// upstream still holds the request, and completion stamps last-activity.
func TestOnDemandTracksActivity(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, "done")
	}))
	defer upstream.Close()

	fake := newFakeUpstream(process.StateRunning)
	h := onDemandHandler(t, testProject(serverPort(t, upstream)), fake)
	tr := tracker(t, h)
	front := httptest.NewServer(h)
	defer front.Close()

	if got := tr.LastActivity(); !got.IsZero() {
		t.Errorf("LastActivity before any request = %v, want zero", got)
	}

	begin := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := front.Client().Get(front.URL + "/slow")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the upstream")
	}
	if got := tr.Inflight(); got != 1 {
		t.Errorf("Inflight during request = %d, want 1", got)
	}
	if !tr.Parked(time.Now()) {
		t.Error("tracker should be parked while a request is in flight")
	}
	close(release)
	<-done

	deadline := time.Now().Add(5 * time.Second)
	for tr.Inflight() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Inflight after request = %d, want 0", tr.Inflight())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := tr.LastActivity(); got.Before(begin) {
		t.Errorf("LastActivity = %v, want at or after the request began (%v)", got, begin)
	}
}

// TestOnDemandRequestDuringIdleStopWaitsForGateThenColdStarts is the
// idle-stop race resolution: a request that arrives while the idle monitor
// is stopping the process is never forwarded to the dying process — it waits
// on the stop gate, then triggers a fresh startup and is forwarded to the
// new process.
func TestOnDemandRequestDuringIdleStopWaitsForGateThenColdStarts(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		fmt.Fprint(w, "fresh process")
	}))
	defer upstream.Close()

	// State still claims "running" while the gate is armed — exactly the
	// window in which the process is about to receive its stop signal.
	fake := newFakeUpstream(process.StateRunning)
	fake.ensure = func() <-chan error {
		ch := make(chan error, 1)
		fake.state.Store(process.StateRunning)
		ch <- nil
		return ch
	}
	h := onDemandHandler(t, testProject(serverPort(t, upstream)), fake)
	tr := tracker(t, h)

	releaseStop, ok := tr.BeginStop()
	if !ok {
		t.Fatal("BeginStop with no activity should succeed")
	}

	type outcome struct {
		code int
		body string
	}
	results := make(chan outcome, 1)
	front := httptest.NewServer(h)
	defer front.Close()
	go func() {
		resp, err := front.Client().Get(front.URL + "/during-stop")
		if err != nil {
			results <- outcome{code: -1, body: err.Error()}
			return
		}
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		body, _ := io.ReadAll(resp.Body)
		results <- outcome{code: resp.StatusCode, body: string(body)}
	}()

	// While the gate is armed the request must be waiting: no forward, no
	// startup trigger.
	deadline := time.Now().Add(5 * time.Second)
	for tr.Inflight() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("request never arrived at the handler")
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if hits := upstreamHits.Load(); hits != 0 {
		t.Fatalf("request was forwarded to the dying process (%d upstream hits before the stop finished)", hits)
	}
	if calls := fake.ensureCalls.Load(); calls != 0 {
		t.Fatalf("startup was triggered while the stop was still in progress (%d calls)", calls)
	}

	// Finish the stop: the process is gone, the gate opens, and the request
	// cold-starts the project.
	fake.state.Store(process.StateStopped)
	releaseStop()

	select {
	case res := <-results:
		if res.code != http.StatusOK || res.body != "fresh process" {
			t.Errorf("response = %d %q, want 200 from the restarted process", res.code, res.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request never completed after the stop finished")
	}
	if calls := fake.ensureCalls.Load(); calls != 1 {
		t.Errorf("EnsureStartedOnDemand called %d times, want exactly 1 (after the gate opened)", calls)
	}
}

// BenchmarkOnDemandHotPath measures per-request overhead of the running-state
// fast path (run with -race to shake out contention).
func BenchmarkOnDemandHotPath(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()

	fake := newFakeUpstream(process.StateRunning)
	port := 0
	if _, err := fmt.Sscanf(upstream.Listener.Addr().String(), "127.0.0.1:%d", &port); err != nil {
		b.Fatal(err)
	}
	front := httptest.NewServer(NewOnDemand(testProject(port), fake, idle.NewTracker(), discardLogger()))
	defer front.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := http.Get(front.URL + "/")
			if err != nil {
				b.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	})
}
