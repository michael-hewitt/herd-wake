package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/process"
	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// dialWS opens a WebSocket to a test server, failing the test on handshake
// problems.
func dialWS(t *testing.T, ts *httptest.Server, path string) *testproc.WSClient {
	t.Helper()
	c, err := testproc.DialWS(ts.Listener.Addr().String(), "dashboard.test", path, 10*time.Second)
	if err != nil {
		t.Fatalf("websocket handshake through the proxy: %v", err)
	}
	return c
}

// echoRoundTrip sends msg and expects it echoed back.
func echoRoundTrip(t *testing.T, c *testproc.WSClient, msg string) {
	t.Helper()
	if err := c.WriteText(msg); err != nil {
		t.Fatalf("write %q: %v", msg, err)
	}
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadText()
	if err != nil {
		t.Fatalf("read echo of %q: %v", msg, err)
	}
	if got != msg {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
}

// TestProxyWebSocketEcho: the plain forwarding proxy passes the WebSocket
// upgrade and bidirectional frames through untouched — dial, upgrade,
// exchange frames both directions, close handshake.
func TestProxyWebSocketEcho(t *testing.T) {
	upstream := httptest.NewServer(testproc.WSEchoHandler())
	defer upstream.Close()

	front := httptest.NewServer(New(testProject(serverPort(t, upstream)), discardLogger()))
	defer front.Close()

	c := dialWS(t, front, "/ws")
	for i := range 3 {
		echoRoundTrip(t, c, fmt.Sprintf("frame %d both ways", i))
	}
	if err := c.Close(); err != nil {
		t.Errorf("close handshake: %v", err)
	}
}

// TestProxyWebSocketUpstreamDisconnectClosesClient: when the upstream side
// of an established tunnel dies, the proxy tears the tunnel down and the
// client sees the connection close promptly (not a hang).
func TestProxyWebSocketUpstreamDisconnectClosesClient(t *testing.T) {
	upstream := httptest.NewServer(testproc.WSEchoHandler())
	defer upstream.Close()

	front := httptest.NewServer(New(testProject(serverPort(t, upstream)), discardLogger()))
	defer front.Close()

	c := dialWS(t, front, "/hmr")
	defer c.Abort() //nolint:errcheck // test cleanup
	echoRoundTrip(t, c, "established")

	// Make the upstream drop the TCP connection mid-tunnel, without any
	// close handshake.
	if err := c.WriteText(testproc.WSDropMessage); err != nil {
		t.Fatalf("trigger upstream drop: %v", err)
	}

	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if msg, err := c.ReadText(); err == nil {
		t.Fatalf("read after upstream disconnect = %q, want a connection error", msg)
	} else if nerr, ok := err.(interface{ Timeout() bool }); ok && nerr.Timeout() {
		t.Fatalf("client connection was not closed after upstream disconnect (read timed out): %v", err)
	}
}

// TestOnDemandWebSocketColdStart: an upgrade request to a not-running
// project triggers the startup, is held until ready, and then completes the
// upgrade against the fresh upstream.
func TestOnDemandWebSocketColdStart(t *testing.T) {
	upstream := httptest.NewServer(testproc.WSEchoHandler())
	defer upstream.Close()

	fake := newFakeUpstream(process.StateStopped)
	fake.ensure = func() <-chan error {
		ch := make(chan error, 1)
		go func() {
			time.Sleep(150 * time.Millisecond)
			fake.state.Store(process.StateRunning)
			ch <- nil
		}()
		return ch
	}
	front := httptest.NewServer(onDemandHandler(t, testProject(serverPort(t, upstream)), fake))
	defer front.Close()

	c := dialWS(t, front, "/ws")
	echoRoundTrip(t, c, "hello after cold start")
	if err := c.Close(); err != nil {
		t.Errorf("close handshake: %v", err)
	}
	if calls := fake.ensureCalls.Load(); calls != 1 {
		t.Errorf("EnsureStartedOnDemand called %d times, want 1", calls)
	}
}

// TestOnDemandWebSocketCountsPersistentConnection: with keep-alive enabled
// (the default) an open WebSocket occupies the persistent-connection slot —
// parking the idle countdown — for exactly as long as the tunnel is open,
// and closing it stamps last-activity.
func TestOnDemandWebSocketCountsPersistentConnection(t *testing.T) {
	upstream := httptest.NewServer(testproc.WSEchoHandler())
	defer upstream.Close()

	fake := newFakeUpstream(process.StateRunning)
	h := onDemandHandler(t, testProject(serverPort(t, upstream)), fake)
	tr := tracker(t, h)
	front := httptest.NewServer(h)
	defer front.Close()

	begin := time.Now()
	c := dialWS(t, front, "/ws")
	echoRoundTrip(t, c, "counted")

	if got := tr.Persistent(); got != 1 {
		t.Errorf("Persistent with an open socket = %d, want 1", got)
	}
	if got := tr.Inflight(); got != 0 {
		t.Errorf("Inflight with an open socket = %d, want 0 (upgrades use the persistent slot)", got)
	}
	if !tr.Parked(time.Now()) {
		t.Error("tracker should be parked while a WebSocket is open")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close handshake: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for tr.Persistent() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Persistent after close = %d, want 0", tr.Persistent())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if la := tr.LastActivity(); la.Before(begin) {
		t.Errorf("LastActivity after close = %v, want at or after the connection began (%v)", la, begin)
	}
}

// TestOnDemandWebSocketKeepAliveDisabled: with websockets_keep_alive false
// the upgrade only bumps last-activity — once the tunnel is established
// nothing is parked, so an idle stop may proceed with the socket open.
func TestOnDemandWebSocketKeepAliveDisabled(t *testing.T) {
	upstream := httptest.NewServer(testproc.WSEchoHandler())
	defer upstream.Close()

	p := testProject(serverPort(t, upstream))
	keepAlive := false
	p.WebSocketsKeepAlive = &keepAlive
	fake := newFakeUpstream(process.StateRunning)
	h := onDemandHandler(t, p, fake)
	tr := tracker(t, h)
	front := httptest.NewServer(h)
	defer front.Close()

	begin := time.Now()
	c := dialWS(t, front, "/ws")
	echoRoundTrip(t, c, "still works") // tunnel fully established

	if got := tr.Persistent(); got != 0 {
		t.Errorf("Persistent = %d, want 0 with keep-alive disabled", got)
	}
	if got := tr.Inflight(); got != 0 {
		t.Errorf("Inflight = %d, want 0 once the tunnel is established", got)
	}
	if tr.Parked(time.Now()) {
		t.Error("tracker must not be parked by a WebSocket when keep-alive is disabled")
	}
	if la := tr.LastActivity(); la.Before(begin) {
		t.Errorf("LastActivity = %v, want the upgrade recorded as momentary activity (>= %v)", la, begin)
	}
	// An idle stop may begin right now, socket open or not.
	release, ok := tr.BeginStop()
	if !ok {
		t.Fatal("BeginStop with only a non-keep-alive WebSocket open should succeed")
	}
	release()

	echoRoundTrip(t, c, "tunnel unaffected by accounting")
	if err := c.Close(); err != nil {
		t.Errorf("close handshake: %v", err)
	}
}

// TestOnDemandWebSocketDuringIdleStopWaitsForGate: an upgrade arriving while
// an idle stop is in progress is not forwarded to the dying process — it
// waits on the stop gate, then cold-starts and completes the upgrade.
func TestOnDemandWebSocketDuringIdleStopWaitsForGate(t *testing.T) {
	upstream := httptest.NewServer(testproc.WSEchoHandler())
	defer upstream.Close()

	fake := newFakeUpstream(process.StateRunning)
	fake.ensure = func() <-chan error {
		ch := make(chan error, 1)
		fake.state.Store(process.StateRunning)
		ch <- nil
		return ch
	}
	h := onDemandHandler(t, testProject(serverPort(t, upstream)), fake)
	tr := tracker(t, h)
	front := httptest.NewServer(h)
	defer front.Close()

	releaseStop, ok := tr.BeginStop()
	if !ok {
		t.Fatal("BeginStop with no activity should succeed")
	}

	type dialResult struct {
		c   *testproc.WSClient
		err error
	}
	results := make(chan dialResult, 1)
	go func() {
		c, err := testproc.DialWS(front.Listener.Addr().String(), "dashboard.test", "/ws", 10*time.Second)
		results <- dialResult{c, err}
	}()

	// The upgrade must be waiting on the gate: its persistent slot is
	// visible, but no startup was triggered.
	deadline := time.Now().Add(5 * time.Second)
	for tr.Persistent() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("upgrade request never arrived at the handler")
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if calls := fake.ensureCalls.Load(); calls != 0 {
		t.Fatalf("startup was triggered while the stop was still in progress (%d calls)", calls)
	}

	// Finish the stop: the gate opens and the upgrade cold-starts.
	fake.state.Store(process.StateStopped)
	releaseStop()

	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("handshake after the stop finished: %v", res.err)
		}
		echoRoundTrip(t, res.c, "fresh process")
		if err := res.c.Close(); err != nil {
			t.Errorf("close handshake: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake never completed after the stop finished")
	}
	if calls := fake.ensureCalls.Load(); calls != 1 {
		t.Errorf("EnsureStartedOnDemand called %d times, want exactly 1 (after the gate opened)", calls)
	}
}

// TestOnDemandWebSocketNoGoroutineLeaks: repeated connect/disconnect cycles
// — clean close handshakes and abrupt client drops alike — leave no
// goroutines behind once the tunnels have settled.
func TestOnDemandWebSocketNoGoroutineLeaks(t *testing.T) {
	upstream := httptest.NewServer(testproc.WSEchoHandler())
	defer upstream.Close()

	fake := newFakeUpstream(process.StateRunning)
	h := onDemandHandler(t, testProject(serverPort(t, upstream)), fake)
	tr := tracker(t, h)
	front := httptest.NewServer(h)
	defer front.Close()

	cycle := func(abrupt bool) {
		c := dialWS(t, front, "/ws")
		echoRoundTrip(t, c, "leak check")
		if abrupt {
			if err := c.Abort(); err != nil {
				t.Fatalf("abort: %v", err)
			}
			return
		}
		if err := c.Close(); err != nil {
			t.Fatalf("close handshake: %v", err)
		}
	}

	// Warm up so lazily created runtime/server goroutines exist before the
	// baseline is taken, then let the warm-up tunnels' goroutines drain.
	for range 3 {
		cycle(false)
	}
	baseline := settledGoroutines(t, 0, time.Second)

	for i := range 20 {
		cycle(i%2 == 1)
	}

	// Every tunnel torn down: the persistent count and the goroutine count
	// must both return to their pre-cycle levels (allow settling time — the
	// second copy goroutine of each tunnel exits shortly after the first).
	deadline := time.Now().Add(10 * time.Second)
	for tr.Persistent() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Persistent after all disconnects = %d, want 0", tr.Persistent())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := settledGoroutines(t, baseline, 10*time.Second); got > baseline {
		t.Errorf("goroutines after 20 connect/disconnect cycles = %d, want <= baseline %d (leak)", got, baseline)
	}
}

// settledGoroutines polls until the goroutine count drops to target or the
// timeout passes, returning the last count observed (target 0 just watches
// the count settle for the whole timeout).
func settledGoroutines(t *testing.T, target int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	n := runtime.NumGoroutine()
	for n > target && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

// TestIsUpgrade covers the header combinations that must (and must not) be
// routed into upgrade accounting.
func TestIsUpgrade(t *testing.T) {
	tests := []struct {
		name       string
		connection string
		upgrade    string
		want       bool
	}{
		{"websocket", "Upgrade", "websocket", true},
		{"case-insensitive tokens", "keep-alive, UPGRADE", "WebSocket", true},
		{"other protocols count too", "Upgrade", "h2c", true},
		{"plain request", "", "", false},
		{"keep-alive only", "keep-alive", "", false},
		{"upgrade header without connection token", "keep-alive", "websocket", false},
		{"connection token without upgrade header", "Upgrade", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://dashboard.test/", nil)
			if tt.connection != "" {
				r.Header.Set("Connection", tt.connection)
			}
			if tt.upgrade != "" {
				r.Header.Set("Upgrade", tt.upgrade)
			}
			if got := isUpgrade(r); got != tt.want {
				t.Errorf("isUpgrade(Connection=%q, Upgrade=%q) = %v, want %v", tt.connection, tt.upgrade, got, tt.want)
			}
		})
	}
}
