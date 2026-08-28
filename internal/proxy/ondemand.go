package proxy

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/process"
)

// diagnosticLogLines is how many recent process-output lines a 503
// diagnostic includes.
const diagnosticLogLines = 30

// Upstream is the view of a project's process supervisor the on-demand proxy
// needs: a lock-free state check for the hot path, the single-flight startup
// trigger for cold starts, and the snapshot/logs that feed 503 diagnostics.
// *process.Supervisor implements it.
type Upstream interface {
	// State returns the current lifecycle state without lifecycle locking.
	State() string
	// EnsureStartedOnDemand triggers (or joins) a request-triggered startup
	// and yields its outcome; during retry backoff it yields a
	// *process.BackoffError immediately.
	EnsureStartedOnDemand() <-chan error
	// Snapshot returns lifecycle details for diagnostics.
	Snapshot() process.Snapshot
	// Logs returns up to n recent process-output lines.
	Logs(n int) []string
}

// Activity is the per-project activity tracking the on-demand proxy feeds:
// every request is bracketed by RequestBegan/RequestEnded (the in-flight
// count and last-completed timestamp drive idle shutdown), and StopGate
// keeps requests away from a process the idle monitor is stopping.
// *idle.Tracker implements it.
type Activity interface {
	// RequestBegan marks one request in flight. The proxy calls it before
	// anything else — including before the lock-free state check — so an
	// idle stop that observes zero in-flight requests can never race this
	// request into the dying process.
	RequestBegan()
	// RequestEnded records the request's completion and releases its
	// in-flight slot.
	RequestEnded()
	// StopGate returns a channel closed when the in-progress idle stop has
	// finished, or nil when no idle stop is in progress.
	StopGate() <-chan struct{}
}

// onDemand wraps the forwarding reverse proxy with request-triggered
// startup: requests to a running project go straight to the proxy after a
// lock-free state check; requests to a not-running project trigger a
// (single-flight) startup and are held — body untouched — until the project
// is ready, then forwarded. Startup failure, retry backoff, or a breached
// hold limit yields a 503 diagnostic.
type onDemand struct {
	project  *config.Project
	upstream Upstream
	activity Activity
	forward  http.Handler
	logger   *log.Logger

	// maxWait bounds how long one request may be held while the project
	// starts.
	maxWait time.Duration
	// maxHeld bounds how many requests may be held at once; held is the
	// current count.
	maxHeld int64
	held    atomic.Int64
}

// NewOnDemand returns the request-triggered-start proxy handler for one
// project. Every request is reported to activity, which drives the
// project's idle shutdown. Hold bounds come from the project's
// hold_max_wait_seconds and hold_max_requests (already defaulted by
// config.Load; unset values fall back to the documented defaults here so
// hand-built configs stay safe).
func NewOnDemand(p *config.Project, upstream Upstream, activity Activity, logger *log.Logger) http.Handler {
	maxWait := time.Duration(p.HoldMaxWaitSeconds) * time.Second
	if maxWait <= 0 {
		maxWait = time.Duration(p.StartupTimeoutSeconds+config.DefaultHoldWaitBufferSeconds) * time.Second
	}
	if maxWait <= 0 {
		maxWait = time.Duration(config.DefaultStartupTimeoutSeconds+config.DefaultHoldWaitBufferSeconds) * time.Second
	}
	maxHeld := int64(p.HoldMaxRequests)
	if maxHeld <= 0 {
		maxHeld = config.DefaultHoldMaxRequests
	}
	return &onDemand{
		project:  p,
		upstream: upstream,
		activity: activity,
		forward:  New(p, logger),
		logger:   logger,
		maxWait:  maxWait,
		maxHeld:  maxHeld,
	}
}

func (h *onDemand) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every request counts as activity — even one that ends up denied — and
	// stays in flight for the whole handler, so a long-running request or
	// response stream parks the idle countdown indefinitely. RequestBegan
	// runs before the gate/state checks below (see Activity).
	h.activity.RequestBegan()
	defer h.activity.RequestEnded()

	// An idle stop in progress: the process is being torn down, so this
	// request must not reach it. Wait for the stop to finish — it is
	// bounded by the project's shutdown timeout plus the force-kill drain —
	// then fall through to the cold path, which starts the project again.
	if gate := h.activity.StopGate(); gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return // the client went away while waiting
		}
	}

	// Hot path: a running project forwards immediately. Atomic loads only —
	// no lifecycle lock — so concurrent requests never serialize here.
	if h.upstream.State() == process.StateRunning {
		h.forward.ServeHTTP(w, r)
		return
	}
	h.holdAndForward(w, r)
}

// holdAndForward is the cold path: trigger (or join) the project's startup
// and hold the request until the outcome, bounded by maxWait and maxHeld.
// The request body is never read while holding — it streams to the upstream
// only once the project is ready — so no body buffering (or body-size limit)
// is needed.
func (h *onDemand) holdAndForward(w http.ResponseWriter, r *http.Request) {
	if held := h.held.Add(1); held > h.maxHeld {
		h.held.Add(-1)
		h.deny(w, r, fmt.Sprintf(
			"Too many requests are already waiting for this project to start (limit %d, hold_max_requests). Retry shortly.",
			h.maxHeld))
		return
	}
	defer h.held.Add(-1)

	wait := time.NewTimer(h.maxWait)
	defer wait.Stop()

	select {
	case err := <-h.upstream.EnsureStartedOnDemand():
		if err != nil {
			h.deny(w, r, err.Error())
			return
		}
		h.forward.ServeHTTP(w, r)
	case <-wait.C:
		h.deny(w, r, fmt.Sprintf(
			"Gave up waiting after %s (hold_max_wait_seconds) for the project to become ready.", h.maxWait))
	case <-r.Context().Done():
		// The client went away; there is nobody to answer. The startup (if
		// one is in flight) continues for other waiters.
	}
}

// deny answers 503 with a diagnostic: the reason, the project's lifecycle
// state, its exit summary and last error, and recent process output. Browsers
// (Accept preferring text/html) get a small HTML page; everything else gets
// plain text.
func (h *onDemand) deny(w http.ResponseWriter, r *http.Request, reason string) {
	snap := h.upstream.Snapshot()
	logs := h.upstream.Logs(diagnosticLogLines)
	h.logger.Printf("project %q: 503 for %s %s: %s", h.project.Name, r.Method, r.URL.Path, reason)

	w.Header().Set("Cache-Control", "no-store")
	if prefersHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		if err := diagnosticPage.Execute(w, diagnosticData{
			Project: h.project.Name,
			Reason:  reason,
			State:   snap.State,
			Exit:    snap.LastExit,
			Err:     snap.LastError,
			Logs:    strings.Join(logs, "\n"),
		}); err != nil {
			h.logger.Printf("project %q: render diagnostic page: %v", h.project.Name, err)
		}
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, "herd-wake: project %q is unavailable.\n\n%s\n\nState: %s\n", h.project.Name, reason, snap.State)
	if snap.LastExit != "" {
		fmt.Fprintf(w, "Last exit: %s\n", snap.LastExit)
	}
	if snap.LastError != "" {
		fmt.Fprintf(w, "Last error: %s\n", snap.LastError)
	}
	if len(logs) > 0 {
		fmt.Fprintf(w, "\nRecent output (herd-wake logs %s):\n", h.project.Name)
		for _, line := range logs {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
}

// prefersHTML reports whether the request's Accept header asks for HTML
// (a browser navigation); API clients and curl default to plain text.
func prefersHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// diagnosticData feeds diagnosticPage; every field is escaped by
// html/template.
type diagnosticData struct {
	Project string
	Reason  string
	State   string
	Exit    string
	Err     string
	Logs    string
}

var diagnosticPage = template.Must(template.New("diagnostic").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>herd-wake: {{.Project}} is unavailable</title>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 2rem auto; max-width: 44rem; padding: 0 1rem; color: #1a1a1a; }
  h1 { font-size: 1.25rem; }
  code { background: #f2f2f2; padding: 0.1em 0.3em; border-radius: 3px; }
  pre { background: #1e1e1e; color: #d6d6d6; padding: 1rem; border-radius: 6px; overflow-x: auto; font-size: 0.8rem; line-height: 1.4; }
  dt { font-weight: 600; }
  dd { margin: 0 0 0.5rem; }
</style>
</head>
<body>
<h1>herd-wake: project &ldquo;{{.Project}}&rdquo; is unavailable</h1>
<p>{{.Reason}}</p>
<dl>
<dt>State</dt><dd>{{.State}}</dd>
{{if .Exit}}<dt>Last exit</dt><dd>{{.Exit}}</dd>{{end}}
{{if .Err}}<dt>Last error</dt><dd>{{.Err}}</dd>{{end}}
</dl>
{{if .Logs}}<p>Recent output (<code>herd-wake logs {{.Project}}</code>):</p>
<pre>{{.Logs}}</pre>{{end}}
<p>Reload to retry, or run <code>herd-wake project:start {{.Project}}</code> to retry immediately.</p>
</body>
</html>
`))
