// Package proxy builds the per-project HTTP reverse proxy that forwards
// traffic from a project's supervisor port to its application port.
//
// Requests arrive from Laravel Herd, which owns the public .test URL and
// terminates HTTPS before forwarding plain HTTP to the supervisor. The proxy
// therefore preserves the inbound Host header and sets the X-Forwarded-*
// headers so the upstream dev server sees the public origin, streams bodies
// in both directions, and propagates client cancellation.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"github.com/michael-hewitt/herd-wake/internal/config"
)

// New returns the plain forwarding reverse proxy for one project: every
// request goes to 127.0.0.1:<application_port>, assuming the upstream dev
// server is already running. If the upstream cannot be reached the handler
// answers 503 with a short diagnostic naming the project and the upstream
// address it tried. NewOnDemand wraps this handler with request-triggered
// startup; the daemon serves that wrapper.
func New(p *config.Project, logger *log.Logger) http.Handler {
	upstream := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.ApplicationPort))

	// The config validated public_url as an absolute http(s) URL; its scheme
	// is the fallback X-Forwarded-Proto when Herd did not send one.
	publicScheme := ""
	if u, err := url.Parse(p.PublicURL); err == nil {
		publicScheme = u.Scheme
	}

	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = upstream

			// Keep the public Host (e.g. dashboard.test) so dev servers that
			// check allowed hosts or build absolute URLs see the real origin.
			r.Out.Host = r.In.Host

			// ReverseProxy strips inbound X-Forwarded-* headers before
			// calling Rewrite; restore Herd's X-Forwarded-For so
			// SetXForwarded appends our client to the existing chain.
			if prior := r.In.Header.Values("X-Forwarded-For"); len(prior) > 0 {
				r.Out.Header["X-Forwarded-For"] = prior
			}
			r.SetXForwarded()

			// Prefer the values Herd set on the public-facing hop: the
			// connection we see is plain loopback HTTP, but the client is
			// really talking https to the .test domain.
			if host := r.In.Header.Get("X-Forwarded-Host"); host != "" {
				r.Out.Header.Set("X-Forwarded-Host", host)
			}
			if proto := r.In.Header.Get("X-Forwarded-Proto"); proto != "" {
				r.Out.Header.Set("X-Forwarded-Proto", proto)
			} else if publicScheme != "" {
				r.Out.Header.Set("X-Forwarded-Proto", publicScheme)
			}
		},

		// Flush response data to the client as soon as the upstream sends it
		// so SSE and other streaming responses (e.g. Vite) work; never
		// buffer whole bodies.
		FlushInterval: -1,

		ErrorLog: logger,

		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The client went away; there is nobody to answer.
			if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			logger.Printf("project %q: proxy to %s failed: %v", p.Name, upstream, err)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "herd-wake: project %q is unavailable.\n\nCould not reach its dev server at http://%s: %v\n\nThe server may have just stopped or crashed; retrying will start it again (see `herd-wake logs %s`).\n",
				p.Name, upstream, err, p.Name)
		},
	}
}
