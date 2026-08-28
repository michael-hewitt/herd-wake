package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// wsProject builds a one-project config whose command is the WebSocket echo
// helper (testproc.ModeWS), with a second-scale idle timeout.
func wsProject(t *testing.T, idleSeconds int) *config.Config {
	t.Helper()
	cfg := idleProject(t, idleSeconds)
	cfg.Projects["dashboard"].Env[testproc.EnvMode] = testproc.ModeWS
	return cfg
}

// dialProjectWS opens a WebSocket through the project's supervisor port. The
// generous handshake timeout covers a cold start.
func dialProjectWS(t *testing.T, cfg *config.Config, path string) *testproc.WSClient {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Projects["dashboard"].SupervisorPort)
	c, err := testproc.DialWS(addr, "dashboard.test", path, 30*time.Second)
	if err != nil {
		t.Fatalf("websocket handshake through the supervisor: %v", err)
	}
	return c
}

// wsEcho sends msg and expects it echoed back.
func wsEcho(t *testing.T, c *testproc.WSClient, msg string) {
	t.Helper()
	if err := c.WriteText(msg); err != nil {
		t.Fatalf("write %q: %v", msg, err)
	}
	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
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

// assertSevered asserts the tunnel dies promptly (an error that is not a
// read timeout) instead of hanging open.
func assertSevered(t *testing.T, c *testproc.WSClient, within time.Duration) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(within)); err != nil {
		t.Fatal(err)
	}
	msg, err := c.ReadText()
	if err == nil {
		t.Fatalf("read on a severed tunnel = %q, want a connection error", msg)
	}
	if nerr, ok := err.(interface{ Timeout() bool }); ok && nerr.Timeout() {
		t.Fatalf("tunnel was not severed within %s (read timed out): %v", within, err)
	}
}

// TestWebSocketColdStartsAndParksIdleStop: a WebSocket handshake to a
// stopped project cold-starts it and completes the upgrade; while the socket
// stays open — with zero HTTP traffic — the project runs past its idle
// timeout; closing the socket starts the idle countdown and the project
// stops.
func TestWebSocketColdStartsAndParksIdleStop(t *testing.T) {
	cfg := wsProject(t, 1)
	socket, _, _ := startDaemon(t, cfg)

	if state := projectStatusByName(t, socket, "dashboard").State; state != StateStopped {
		t.Fatalf("initial state = %q, want %q", state, StateStopped)
	}

	// Cold start via the upgrade request itself.
	c := dialProjectWS(t, cfg, "/hmr")
	wsEcho(t, c, "hello over websocket")
	waitForProjectState(t, socket, "dashboard", StateRunning, 5*time.Second)

	// 2.5x the idle timeout with the socket open and no other traffic: the
	// project must stay running, with no idle stop scheduled.
	time.Sleep(2500 * time.Millisecond)
	p := projectStatusByName(t, socket, "dashboard")
	if p.State != StateRunning {
		t.Fatalf("state with an open WebSocket = %q, want %q", p.State, StateRunning)
	}
	if !p.IdleStopAt.IsZero() {
		t.Errorf("IdleStopAt = %v, want zero while a WebSocket parks the countdown", p.IdleStopAt)
	}
	wsEcho(t, c, "still connected") // the parked project still serves the socket

	// Closing the socket starts a fresh idle window; the stop follows.
	if err := c.Close(); err != nil {
		t.Fatalf("close handshake: %v", err)
	}
	waitForProjectState(t, socket, "dashboard", StateStopped, 10*time.Second)
}

// TestWebSocketKeepAliveDisabledIdleStopsAndSevers: with
// websockets_keep_alive false an open WebSocket does not prevent the idle
// stop, and the stop severs the socket promptly (the process dying closes
// the upstream side and the proxy tears the tunnel down).
func TestWebSocketKeepAliveDisabledIdleStopsAndSevers(t *testing.T) {
	cfg := wsProject(t, 1)
	keepAlive := false
	cfg.Projects["dashboard"].WebSocketsKeepAlive = &keepAlive
	socket, _, _ := startDaemon(t, cfg)

	c := dialProjectWS(t, cfg, "/hmr")
	defer c.Abort() //nolint:errcheck // test cleanup
	wsEcho(t, c, "connected without keep-alive")

	// The open socket counts only as momentary activity: the idle stop must
	// proceed on schedule (and must not wedge on the open tunnel).
	waitForProjectState(t, socket, "dashboard", StateStopped, 10*time.Second)
	assertSevered(t, c, 5*time.Second)
}

// TestManualStopClosesOpenWebSockets: project:stop with open keep-alive
// WebSockets completes without wedging, and the sockets are closed.
func TestManualStopClosesOpenWebSockets(t *testing.T) {
	cfg := wsProject(t, 300) // long idle timeout: only the manual stop acts
	socket, _, _ := startDaemon(t, cfg)

	c := dialProjectWS(t, cfg, "/hmr")
	defer c.Abort() //nolint:errcheck // test cleanup
	wsEcho(t, c, "connected before stop")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stopped, err := control.NewClient(socket).StopProject(ctx, "dashboard")
	if err != nil {
		t.Fatalf("StopProject with an open WebSocket: %v", err)
	}
	if stopped.State != StateStopped {
		t.Fatalf("state after manual stop = %q, want %q", stopped.State, StateStopped)
	}
	assertSevered(t, c, 5*time.Second)
}
