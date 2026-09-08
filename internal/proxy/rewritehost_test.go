package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/process"
	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// hostSeen is what an upstream observed about one request's host headers.
type hostSeen struct {
	host, forwardedHost, forwardedProto string
}

// recordHost wraps next, reporting each request's host headers on seen
// before delegating (a buffered channel keeps the report race-free).
func recordHost(seen chan<- hostSeen, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- hostSeen{
			host:           r.Host,
			forwardedHost:  r.Header.Get("X-Forwarded-Host"),
			forwardedProto: r.Header.Get("X-Forwarded-Proto"),
		}
		if next != nil {
			next.ServeHTTP(w, r)
		}
	})
}

// TestProxyHostPolicy: rewrite_host false forwards the inbound Host
// unchanged; rewrite_host true sends Host: 127.0.0.1:<application_port>.
// Either way X-Forwarded-Host carries the public host.
func TestProxyHostPolicy(t *testing.T) {
	for _, tt := range []struct {
		name    string
		rewrite bool
	}{
		{"preserve inbound host", false},
		{"rewrite to loopback upstream", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seen := make(chan hostSeen, 1)
			upstream := httptest.NewServer(recordHost(seen, nil))
			defer upstream.Close()
			port := serverPort(t, upstream)

			p := testProject(port)
			p.RewriteHost = tt.rewrite
			front := httptest.NewServer(New(p, discardLogger()))
			defer front.Close()

			req, err := http.NewRequest(http.MethodGet, front.URL+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "issue-3265.test"
			resp, err := front.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			got := <-seen
			wantHost := "issue-3265.test"
			if tt.rewrite {
				wantHost = fmt.Sprintf("127.0.0.1:%d", port)
			}
			if got.host != wantHost {
				t.Errorf("upstream Host = %q, want %q", got.host, wantHost)
			}
			if got.forwardedHost != "issue-3265.test" {
				t.Errorf("X-Forwarded-Host = %q, want the public host", got.forwardedHost)
			}
			if got.forwardedProto != "https" {
				t.Errorf("X-Forwarded-Proto = %q, want https (from public_url)", got.forwardedProto)
			}
		})
	}
}

// TestProxyRewriteHostKeepsHerdForwardedHost: with rewrite_host, Herd's own
// X-Forwarded-Host (the public hop) still wins over the inbound Host.
func TestProxyRewriteHostKeepsHerdForwardedHost(t *testing.T) {
	seen := make(chan hostSeen, 1)
	upstream := httptest.NewServer(recordHost(seen, nil))
	defer upstream.Close()
	port := serverPort(t, upstream)

	p := testProject(port)
	p.RewriteHost = true
	front := httptest.NewServer(New(p, discardLogger()))
	defer front.Close()

	req, err := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:7101" // what a loopback hop would carry
	req.Header.Set("X-Forwarded-Host", "issue-3265.test")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	got := <-seen
	if want := fmt.Sprintf("127.0.0.1:%d", port); got.host != want {
		t.Errorf("upstream Host = %q, want %q", got.host, want)
	}
	if got.forwardedHost != "issue-3265.test" {
		t.Errorf("X-Forwarded-Host = %q, want Herd's value preserved", got.forwardedHost)
	}
}

// TestProxyHostPolicyAppliesToWebSocketUpgrade: the Host policy applies to
// upgrade requests too — through the on-demand handler, i.e. exactly the
// path an HMR handshake takes — and the tunnel still works afterwards.
func TestProxyHostPolicyAppliesToWebSocketUpgrade(t *testing.T) {
	for _, tt := range []struct {
		name    string
		rewrite bool
	}{
		{"preserve inbound host", false},
		{"rewrite to loopback upstream", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seen := make(chan hostSeen, 1)
			upstream := httptest.NewServer(recordHost(seen, testproc.WSEchoHandler()))
			defer upstream.Close()
			port := serverPort(t, upstream)

			p := testProject(port)
			p.RewriteHost = tt.rewrite
			front := httptest.NewServer(onDemandHandler(t, p, newFakeUpstream(process.StateRunning)))
			defer front.Close()

			c, err := testproc.DialWS(front.Listener.Addr().String(), "issue-3265.test", "/hmr", 10*time.Second)
			if err != nil {
				t.Fatalf("websocket handshake through the proxy: %v", err)
			}
			echoRoundTrip(t, c, "host policy check")
			if err := c.Close(); err != nil {
				t.Errorf("close handshake: %v", err)
			}

			got := <-seen
			wantHost := "issue-3265.test"
			if tt.rewrite {
				wantHost = fmt.Sprintf("127.0.0.1:%d", port)
			}
			if got.host != wantHost {
				t.Errorf("upgrade Host = %q, want %q", got.host, wantHost)
			}
			if got.forwardedHost != "issue-3265.test" {
				t.Errorf("upgrade X-Forwarded-Host = %q, want the public host", got.forwardedHost)
			}
		})
	}
}
