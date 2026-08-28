package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stubProvider struct {
	status StatusResponse
	logs   LogsResponse
	err    error // returned by every lifecycle method when set

	lastAction   string
	lastName     string
	lastMaxLines int
	lastTTL      time.Duration
}

func (s *stubProvider) Status() StatusResponse { return s.status }

func (s *stubProvider) action(action, name string) (ProjectStatus, error) {
	s.lastAction, s.lastName = action, name
	if s.err != nil {
		return ProjectStatus{}, s.err
	}
	return ProjectStatus{Name: name, State: "running", PID: 4321}, nil
}

func (s *stubProvider) StartProject(_ context.Context, name string) (ProjectStatus, error) {
	return s.action("start", name)
}

func (s *stubProvider) StopProject(_ context.Context, name string) (ProjectStatus, error) {
	return s.action("stop", name)
}

func (s *stubProvider) RestartProject(_ context.Context, name string) (ProjectStatus, error) {
	return s.action("restart", name)
}

func (s *stubProvider) LeaseProject(_ context.Context, name string, ttl time.Duration) (ProjectStatus, error) {
	s.lastTTL = ttl
	return s.action("lease", name)
}

func (s *stubProvider) ReleaseProjectLease(_ context.Context, name string) (ProjectStatus, error) {
	return s.action("release", name)
}

func (s *stubProvider) ProjectLogs(name string, maxLines int) (LogsResponse, error) {
	s.lastAction, s.lastName, s.lastMaxLines = "logs", name, maxLines
	if s.err != nil {
		return LogsResponse{}, s.err
	}
	return s.logs, nil
}

// startServer serves the control API for provider on a test unix socket and
// returns a client for it.
func startServer(t *testing.T, provider Provider) *Client {
	t.Helper()
	socket := testSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	server := &http.Server{Handler: NewHandler(provider)}
	go server.Serve(listener) //nolint:errcheck // closed via server.Close
	t.Cleanup(func() { _ = server.Close() })
	return NewClient(socket)
}

// testSocketPath returns a unix socket path short enough for the platform's
// sun_path limit (104 bytes on macOS).
func testSocketPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.sock")
	if len(path) < 100 {
		return path
	}
	dir, err := os.MkdirTemp("", "hw")
	if err != nil {
		t.Fatalf("make short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "c.sock")
}

func TestClientStatusRoundTrip(t *testing.T) {
	socket := testSocketPath(t)
	want := StatusResponse{
		Version:       "1.2.3",
		PID:           4242,
		StartedAt:     time.Now().Add(-90 * time.Second).UTC(),
		UptimeSeconds: 90,
		Projects: []ProjectStatus{{
			Name:            "dashboard",
			PublicURL:       "https://dashboard.test",
			SupervisorPort:  7101,
			ApplicationPort: 17101,
			State:           "stopped",
		}},
	}

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	server := &http.Server{Handler: NewHandler(&stubProvider{status: want})}
	go server.Serve(listener) //nolint:errcheck // closed via server.Close
	defer server.Close()      //nolint:errcheck // test cleanup

	got, err := NewClient(socket).Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}

	if got.Version != want.Version || got.PID != want.PID || got.UptimeSeconds != want.UptimeSeconds {
		t.Errorf("Status() = %+v, want %+v", got, want)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
	if len(got.Projects) != 1 || got.Projects[0] != want.Projects[0] {
		t.Errorf("Projects = %+v, want %+v", got.Projects, want.Projects)
	}
	if got.Uptime() != 90*time.Second {
		t.Errorf("Uptime() = %v, want 90s", got.Uptime())
	}
}

func TestClientStatusNoDaemon(t *testing.T) {
	socket := testSocketPath(t) // nothing listens here

	_, err := NewClient(socket).Status(context.Background())

	if err == nil {
		t.Fatal("Status() should fail when no daemon is listening")
	}
	if !strings.Contains(err.Error(), socket) {
		t.Errorf("error should name the socket path %q; got: %v", socket, err)
	}
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("error should match ErrDaemonUnreachable; got: %v", err)
	}
}

func TestClientProjectActionsRoundTrip(t *testing.T) {
	stub := &stubProvider{}
	client := startServer(t, stub)

	actions := []struct {
		name string
		call func(ctx context.Context, name string) (*ProjectStatus, error)
	}{
		{"start", client.StartProject},
		{"stop", client.StopProject},
		{"restart", client.RestartProject},
		{"release", client.ReleaseProjectLease},
	}
	for _, action := range actions {
		status, err := action.call(context.Background(), "dash board") // space: exercises path escaping
		if err != nil {
			t.Fatalf("%s error: %v", action.name, err)
		}
		if stub.lastAction != action.name || stub.lastName != "dash board" {
			t.Errorf("%s reached provider as (%q, %q)", action.name, stub.lastAction, stub.lastName)
		}
		if status.Name != "dash board" || status.State != "running" || status.PID != 4321 {
			t.Errorf("%s status = %+v", action.name, status)
		}
	}
}

func TestClientLeaseRoundTrip(t *testing.T) {
	stub := &stubProvider{}
	client := startServer(t, stub)

	status, err := client.LeaseProject(context.Background(), "dash board", 45*time.Minute)
	if err != nil {
		t.Fatalf("LeaseProject error: %v", err)
	}

	if stub.lastAction != "lease" || stub.lastName != "dash board" || stub.lastTTL != 45*time.Minute {
		t.Errorf("provider got (%q, %q, %s), want (lease, dash board, 45m)",
			stub.lastAction, stub.lastName, stub.lastTTL)
	}
	if status.Name != "dash board" {
		t.Errorf("status = %+v", status)
	}
}

func TestClientLeaseRejectsBadTTL(t *testing.T) {
	stub := &stubProvider{}
	client := startServer(t, stub)

	_, err := client.LeaseProject(context.Background(), "dashboard", -time.Minute)

	if err == nil {
		t.Fatal("LeaseProject with a negative ttl should fail")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("error = %v, want a 400 APIError", err)
	}
	if !strings.Contains(apiErr.Message, "ttl") {
		t.Errorf("Message = %q, want it to explain the ttl", apiErr.Message)
	}
	if stub.lastAction == "lease" {
		t.Error("a bad ttl must be rejected before reaching the provider")
	}
}

func TestClientLogsRoundTrip(t *testing.T) {
	stub := &stubProvider{logs: LogsResponse{
		Name:    "dashboard",
		LogFile: "/tmp/dashboard.log",
		Lines:   []string{"one", "two"},
	}}
	client := startServer(t, stub)

	logs, err := client.Logs(context.Background(), "dashboard", 25)
	if err != nil {
		t.Fatalf("Logs() error: %v", err)
	}

	if stub.lastName != "dashboard" || stub.lastMaxLines != 25 {
		t.Errorf("provider got (name=%q, maxLines=%d), want (dashboard, 25)", stub.lastName, stub.lastMaxLines)
	}
	if logs.LogFile != "/tmp/dashboard.log" || len(logs.Lines) != 2 || logs.Lines[0] != "one" {
		t.Errorf("Logs() = %+v", logs)
	}
}

func TestClientUnknownProjectIs404(t *testing.T) {
	stub := &stubProvider{err: fmt.Errorf("%w %q", ErrUnknownProject, "nope")}
	client := startServer(t, stub)

	_, err := client.StartProject(context.Background(), "nope")

	if err == nil {
		t.Fatal("StartProject(unknown) should fail")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T (%v), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Message, `unknown project "nope"`) {
		t.Errorf("Message = %q, want it to name the unknown project", apiErr.Message)
	}
	if errors.Is(err, ErrDaemonUnreachable) {
		t.Error("an API error must not match ErrDaemonUnreachable")
	}
}
