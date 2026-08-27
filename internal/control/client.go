package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client talks to a running daemon over its unix control socket.
type Client struct {
	socketPath string
	httpClient *http.Client
}

// NewClient returns a client for the daemon control socket at socketPath.
// It does not connect until a request is made.
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
			Timeout: 5 * time.Second,
		},
	}
}

// Status fetches the daemon status. A connection error (no daemon listening
// on the socket) is returned wrapped, so callers can present a "daemon not
// running" message.
func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	// The host is a placeholder: the transport always dials the unix socket.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://herd-wake/v1/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon on %s: %w", c.socketPath, err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to recover from a close error on a read-only body

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("daemon answered %s: %s", resp.Status, string(body))
	}

	var status StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode daemon status: %w", err)
	}
	return &status, nil
}
