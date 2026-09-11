package daemon

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/michael-hewitt/herd-wake/internal/proxy"
)

// router is the handler every project listener serves. It maps a request
// to a project's handler by the request's Host header: exact hosts for
// projects that set host, a catch-all slot for a project that owns the
// listener outright (the one-port-per-project case, which never parses
// the Host at all), and — on a wildcard entry's listener — a resolver that
// materialises unknown <label>.<base_domain> hosts on demand. A hostname
// nothing claims gets a 404 diagnostic naming it: never a hang, never
// another project's server.
//
// The routing table is immutable and swapped atomically (copy-on-write),
// so the request hot path is one atomic load and one map lookup; table
// updates are rare (reloads, wildcard materialisation) and serialized by
// mu.
type router struct {
	addr   string
	logger *log.Logger
	table  atomic.Pointer[routeTable]
	mu     sync.Mutex
	// resolver materialises unknown hosts on a wildcard entry's listener;
	// nil elsewhere. Set before the listener serves, never changed.
	resolver *resolver
}

// routeTable is one immutable snapshot of a router's routes.
type routeTable struct {
	hosts    map[string]http.Handler
	catchAll http.Handler
}

func newRouter(addr string, logger *log.Logger) *router {
	rt := &router{addr: addr, logger: logger}
	rt.table.Store(&routeTable{hosts: map[string]http.Handler{}})
	return rt
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := rt.table.Load()
	if t.catchAll != nil {
		t.catchAll.ServeHTTP(w, r)
		return
	}
	host := requestHost(r)
	if h := t.hosts[host]; h != nil {
		h.ServeHTTP(w, r)
		return
	}
	if rt.resolver != nil {
		rt.resolver.serve(w, r, host)
		return
	}
	rt.notFound(w, r, host, t)
}

// update swaps in a modified copy of the table.
func (rt *router) update(change func(t *routeTable)) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	old := rt.table.Load()
	next := &routeTable{hosts: make(map[string]http.Handler, len(old.hosts)+1), catchAll: old.catchAll}
	for host, h := range old.hosts {
		next.hosts[host] = h
	}
	change(next)
	rt.table.Store(next)
}

// setHost routes host to h.
func (rt *router) setHost(host string, h http.Handler) {
	rt.update(func(t *routeTable) { t.hosts[host] = h })
}

// removeHost stops routing host. It is idempotent.
func (rt *router) removeHost(host string) {
	rt.update(func(t *routeTable) { delete(t.hosts, host) })
}

// setCatchAll routes every request to h regardless of Host (nil clears it).
func (rt *router) setCatchAll(h http.Handler) {
	rt.update(func(t *routeTable) { t.catchAll = h })
}

// notFound answers a request for a hostname no project on this listener
// claims.
func (rt *router) notFound(w http.ResponseWriter, r *http.Request, host string, t *routeTable) {
	known := make([]string, 0, len(t.hosts))
	for h := range t.hosts {
		known = append(known, h)
	}
	sort.Strings(known)
	rt.logger.Printf("listener %s: 404 for %s %s: no project for host %q", rt.addr, r.Method, r.URL.Path, host)
	served := "no project at all"
	if len(known) > 0 {
		served = strings.Join(known, ", ")
	}
	proxy.WriteDiagnostic(w, r, proxy.Diagnostic{
		Status: http.StatusNotFound,
		Title:  fmt.Sprintf("no project for host %q", host),
		Reason: fmt.Sprintf("The herd-wake listener on %s routes requests by hostname and has no project registered for %q. Hosts served here: %s.",
			rt.addr, host, served),
		Hint: fmt.Sprintf("Add a project with `host: %s` to the configuration and run `herd-wake reload`, or point the request at one of the hosts above.", host),
	})
}

// requestHost normalises a request's Host header for routing: the port is
// stripped, the name lowercased, and a trailing dot removed.
func requestHost(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}
