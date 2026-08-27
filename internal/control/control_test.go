package control

import (
	"context"
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
}

func (s *stubProvider) Status() StatusResponse { return s.status }

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
}
