package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
)

// testProject returns a minimal valid project whose upstream is
// 127.0.0.1:appPort.
func testProject(appPort int) *config.Project {
	return &config.Project{
		Name:            "dashboard",
		PublicURL:       "https://dashboard.test",
		SupervisorPort:  7101,
		ApplicationPort: appPort,
		ListenHost:      "127.0.0.1",
	}
}

// serverPort extracts the TCP port an httptest server is listening on.
func serverPort(t *testing.T, ts *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port := u.Port()
	if port == "" {
		t.Fatalf("test server URL %q has no port", ts.URL)
	}
	var n int
	if _, err := fmt.Sscanf(port, "%d", &n); err != nil {
		t.Fatalf("parse port %q: %v", port, err)
	}
	return n
}

// freePort reserves and releases a loopback port so the tests can point at
// an address where nothing is listening (or bind it themselves).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}

func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestProxyPreservesRequestAndResponse(t *testing.T) {
	type seen struct {
		method, path, query, header, host, body string
	}
	var got seen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = seen{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			header: r.Header.Get("X-Custom"),
			host:   r.Host,
			body:   string(body),
		}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, "created!")
	}))
	defer upstream.Close()

	front := httptest.NewServer(New(testProject(serverPort(t, upstream)), discardLogger()))
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/api/items?a=1&b=two", strings.NewReader("hello body"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "dashboard.test"
	req.Header.Set("X-Custom", "custom-value")
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Errorf("response header X-Upstream = %q, want %q", resp.Header.Get("X-Upstream"), "yes")
	}
	if string(respBody) != "created!" {
		t.Errorf("response body = %q, want %q", respBody, "created!")
	}
	want := seen{
		method: "POST",
		path:   "/api/items",
		query:  "a=1&b=two",
		header: "custom-value",
		host:   "dashboard.test",
		body:   "hello body",
	}
	if got != want {
		t.Errorf("upstream saw %+v, want %+v", got, want)
	}
}

func TestProxySetsForwardedHeadersFromPublicURL(t *testing.T) {
	var proto, host, forwardedFor string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto = r.Header.Get("X-Forwarded-Proto")
		host = r.Header.Get("X-Forwarded-Host")
		forwardedFor = r.Header.Get("X-Forwarded-For")
	}))
	defer upstream.Close()

	front := httptest.NewServer(New(testProject(serverPort(t, upstream)), discardLogger()))
	defer front.Close()

	req, err := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "dashboard.test"
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// No X-Forwarded-Proto came in, so the public_url scheme wins.
	if proto != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want %q (from public_url)", proto, "https")
	}
	if host != "dashboard.test" {
		t.Errorf("X-Forwarded-Host = %q, want %q", host, "dashboard.test")
	}
	if !strings.HasPrefix(forwardedFor, "127.0.0.1") {
		t.Errorf("X-Forwarded-For = %q, want the loopback client address", forwardedFor)
	}
}

func TestProxyChainsHerdForwardedHeaders(t *testing.T) {
	var proto, host, forwardedFor string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto = r.Header.Get("X-Forwarded-Proto")
		host = r.Header.Get("X-Forwarded-Host")
		forwardedFor = r.Header.Get("X-Forwarded-For")
	}))
	defer upstream.Close()

	front := httptest.NewServer(New(testProject(serverPort(t, upstream)), discardLogger()))
	defer front.Close()

	req, err := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "dashboard.test")
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if proto != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want %q (Herd's value preserved)", proto, "https")
	}
	if host != "dashboard.test" {
		t.Errorf("X-Forwarded-Host = %q, want %q (Herd's value preserved)", host, "dashboard.test")
	}
	if !strings.HasPrefix(forwardedFor, "203.0.113.9, ") {
		t.Errorf("X-Forwarded-For = %q, want the original client prepended", forwardedFor)
	}
}

func TestProxyUpstreamDownReturns503Diagnostic(t *testing.T) {
	port := freePort(t) // nothing listens here
	front := httptest.NewServer(New(testProject(port), discardLogger()))
	defer front.Close()

	resp, err := front.Client().Get(front.URL + "/")
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	text := string(body)
	if !strings.Contains(text, `"dashboard"`) {
		t.Errorf("503 body should name the project; got:\n%s", text)
	}
	wantAddr := fmt.Sprintf("127.0.0.1:%d", port)
	if !strings.Contains(text, wantAddr) {
		t.Errorf("503 body should name the upstream address %s; got:\n%s", wantAddr, text)
	}
}

func TestProxyStreamsResponseBody(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, "data: first")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
		fmt.Fprintln(w, "data: second")
	}))
	defer upstream.Close()
	defer close(release)

	front := httptest.NewServer(New(testProject(serverPort(t, upstream)), discardLogger()))
	defer front.Close()

	resp, err := front.Client().Get(front.URL + "/events")
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	// The first chunk must arrive while the upstream handler is still
	// blocked, i.e. the proxy must not buffer the whole body.
	type lineResult struct {
		line string
		err  error
	}
	lines := make(chan lineResult, 1)
	go func() {
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		lines <- lineResult{line, err}
	}()
	select {
	case got := <-lines:
		if got.err != nil {
			t.Fatalf("read first streamed line: %v", got.err)
		}
		if strings.TrimSpace(got.line) != "data: first" {
			t.Errorf("first streamed line = %q, want %q", got.line, "data: first")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first streamed chunk; the proxy appears to buffer the response")
	}
}

func TestProxyPropagatesClientCancellation(t *testing.T) {
	handlerStarted := make(chan struct{})
	handlerDone := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		<-r.Context().Done()
		handlerDone <- r.Context().Err()
	}))
	defer upstream.Close()

	front := httptest.NewServer(New(testProject(serverPort(t, upstream)), discardLogger()))
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"/slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan struct{})
	go func() {
		resp, err := front.Client().Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		close(requestDone)
	}()

	select {
	case <-handlerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the proxied request")
	}
	cancel()

	select {
	case err := <-handlerDone:
		if err == nil {
			t.Error("upstream request context should be cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client cancellation did not propagate to the upstream")
	}
	<-requestDone
}
