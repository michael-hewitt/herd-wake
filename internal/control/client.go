package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client talks to a running daemon over its unix control socket.
type Client struct {
	socketPath string
	httpClient *http.Client
}

// NewClient returns a client for the daemon control socket at socketPath.
// It does not connect until a request is made. Requests have no built-in
// deadline — lifecycle operations legitimately take as long as a project's
// startup/shutdown timeouts — so callers bound each call with its context.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// APIError is a non-200 answer from the daemon.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string { return e.Message }

// unreachableError wraps a failure to reach the daemon at all. It matches
// ErrDaemonUnreachable so callers can present a "daemon not running" hint.
type unreachableError struct {
	socketPath string
	err        error
}

func (e *unreachableError) Error() string {
	return fmt.Sprintf("connect to daemon on %s: %v", e.socketPath, e.err)
}

func (e *unreachableError) Is(target error) bool { return target == ErrDaemonUnreachable }

func (e *unreachableError) Unwrap() error { return e.err }

// Status fetches the daemon status. When no daemon is listening on the
// socket the error matches ErrDaemonUnreachable.
func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	var status StatusResponse
	if err := c.do(ctx, http.MethodGet, "/v1/status", &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// StartProject asks the daemon to start the named project, returning once it
// is running or startup failed.
func (c *Client) StartProject(ctx context.Context, name string) (*ProjectStatus, error) {
	return c.projectAction(ctx, name, "start")
}

// StopProject asks the daemon to gracefully stop the named project.
func (c *Client) StopProject(ctx context.Context, name string) (*ProjectStatus, error) {
	return c.projectAction(ctx, name, "stop")
}

// RestartProject asks the daemon to stop (if needed) and start the named
// project.
func (c *Client) RestartProject(ctx context.Context, name string) (*ProjectStatus, error) {
	return c.projectAction(ctx, name, "restart")
}

// Logs fetches up to maxLines recent output lines for the named project
// (all buffered lines when maxLines <= 0).
func (c *Client) Logs(ctx context.Context, name string, maxLines int) (*LogsResponse, error) {
	path := "/v1/projects/" + url.PathEscape(name) + "/logs"
	if maxLines > 0 {
		path += "?lines=" + strconv.Itoa(maxLines)
	}
	var logs LogsResponse
	if err := c.do(ctx, http.MethodGet, path, &logs); err != nil {
		return nil, err
	}
	return &logs, nil
}

func (c *Client) projectAction(ctx context.Context, name, action string) (*ProjectStatus, error) {
	path := "/v1/projects/" + url.PathEscape(name) + "/" + action
	var status ProjectStatus
	if err := c.do(ctx, http.MethodPost, path, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// do performs one control API request and decodes the 200 response into out.
// Connection failures come back matching ErrDaemonUnreachable; non-200
// answers come back as *APIError.
func (c *Client) do(ctx context.Context, method, path string, out any) error {
	// The host is a placeholder: the transport always dials the unix socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://herd-wake"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("request to daemon on %s: %w", c.socketPath, ctx.Err())
		}
		return &unreachableError{socketPath: c.socketPath, err: err}
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to recover from a close error on a read-only body

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		message := strings.TrimSpace(string(body))
		var apiErr errorResponse
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
			message = apiErr.Error
		}
		if message == "" {
			message = fmt.Sprintf("daemon answered %s", resp.Status)
		}
		return &APIError{StatusCode: resp.StatusCode, Message: message}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode daemon response for %s: %w", path, err)
	}
	return nil
}
