// Package daemon wires the herd-wake supervisor daemon together: one
// loopback reverse-proxy listener per registered project, a process
// supervisor per project, and the control API on a unix socket. It owns
// listener lifecycle — binding, serving, and clean shutdown including
// control-socket file removal — and guarantees every process group it
// started is terminated before the daemon exits.
//
// The registered project set is live: Reload (POST /v1/reload, `herd-wake
// reload`, or SIGHUP) re-reads the config and applies the difference —
// binding added projects, stopping and unbinding removed ones, and swapping
// changed ones — without disturbing projects whose configuration did not
// change. See reload.go.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/idle"
	"github.com/michael-hewitt/herd-wake/internal/process"
)

// shutdownTimeout bounds how long a listener being shut down (daemon exit,
// or a project removed or rebound by a reload) waits for in-flight
// requests.
const shutdownTimeout = 5 * time.Second

// Daemon is the supervisor daemon: per-project proxy listeners, per-project
// process supervisors, and the control socket.
type Daemon struct {
	// configPath is the main config file, re-read on reload (empty when the
	// daemon was built from an in-memory config, which cannot be reloaded).
	configPath string
	socketPath string
	logDir     string
	logger     *log.Logger
	startedAt  time.Time

	// mu guards states, lastReloadAt, and runCtx. Only the control API and
	// reloads take it: the request hot path runs entirely inside a
	// project's own listener (http.Server -> handlerSwitch -> proxy) and
	// never consults the daemon's table, so requests to running projects
	// are never blocked by a reload.
	mu           sync.RWMutex
	states       map[string]*projectState
	lastReloadAt time.Time
	// runCtx is Run's lifetime: idle monitors and always_on starters (also
	// those created by later reloads) run until it ends.
	runCtx context.Context

	// reloadMu serializes reloads with each other and with daemon shutdown.
	reloadMu sync.Mutex
	// draining is set the moment Run begins shutting down, so a request
	// racing shutdown cannot trigger a fresh startup of a project the
	// daemon is about to stop for good.
	draining atomic.Bool
	// serveErr receives the first real serve failure of any listener; Run
	// shuts the daemon down on it.
	serveErr chan error
}

// onDemandUpstream adapts a project's supervisor for the on-demand proxy:
// once the daemon is draining, request-triggered starts are refused instead
// of respawning a project that daemon shutdown is (or will be) stopping.
type onDemandUpstream struct {
	*process.Supervisor
	draining *atomic.Bool
}

func (u onDemandUpstream) EnsureStartedOnDemand() <-chan error {
	if u.draining.Load() {
		done := make(chan error, 1)
		done <- errors.New("the herd-wake daemon is shutting down")
		return done
	}
	return u.Supervisor.EnsureStartedOnDemand()
}

// New builds a daemon for the given configuration. The control API listens
// on the unix socket at socketPath; project process output is written under
// logDir (one <name>.log per project); diagnostics go to logger. Reload
// re-reads cfg.Path, so a config built in memory (empty Path) cannot be
// reloaded.
func New(cfg *config.Config, socketPath, logDir string, logger *log.Logger) *Daemon {
	d := &Daemon{
		configPath: cfg.Path,
		socketPath: socketPath,
		logDir:     logDir,
		logger:     logger,
		states:     make(map[string]*projectState, len(cfg.Projects)),
		serveErr:   make(chan error, 1),
	}
	for _, name := range cfg.ProjectNames() {
		d.states[name] = d.newProjectState(cfg.Projects[name], idle.NewTracker())
	}
	return d
}

// Run binds every listener and serves until ctx is cancelled (SIGINT/SIGTERM
// in the CLI) or a listener fails. SIGHUP reloads the configuration. On
// return all listeners are closed, every supervised process group is
// stopped, and the control socket file is removed.
//
// If another daemon already answers on the control socket, Run refuses to
// start. A stale socket file (nothing accepting) is removed and replaced.
func (d *Daemon) Run(ctx context.Context) error {
	// Deferred (not just called on the normal path) so supervised process
	// groups are terminated even if the daemon panics.
	defer d.stopAllProjects()

	if err := d.claimSocket(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.socketPath), 0o755); err != nil {
		return fmt.Errorf("create control socket directory: %w", err)
	}

	controlListener, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return fmt.Errorf("listen on control socket %s: %w", d.socketPath, err)
	}
	controlBinding := &binding{
		name:     "control socket",
		listener: controlListener,
		server:   &http.Server{Handler: control.NewHandler(d), ErrorLog: d.logger},
	}

	// Bind every project listener before serving anything: at startup a
	// taken supervisor_port is an error, not a degraded state.
	states := d.sortedStates()
	for _, st := range states {
		b, err := d.bindProject(st.project, d.handler(st))
		if err != nil {
			for _, bound := range states {
				if bound.bind != nil {
					bound.bind.listener.Close() //nolint:errcheck // best-effort cleanup
				}
			}
			controlListener.Close() //nolint:errcheck // best-effort cleanup
			d.removeSocketFile()
			return fmt.Errorf("project %q: %w", st.project.Name, err)
		}
		st.bind = b
		d.logProxying(st.project)
	}

	d.startedAt = time.Now()
	d.logger.Printf("daemon ready: %d project(s), control socket %s", len(states), d.socketPath)

	// Idle monitors and always_on startups run until the daemon begins
	// shutting down; runCtx also ends them when Run exits on a serve error.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	d.mu.Lock()
	d.runCtx = runCtx
	d.mu.Unlock()

	for _, st := range states {
		d.activate(st, runCtx)
		go d.serve(st.bind)
	}
	go d.serve(controlBinding)
	go d.reloadOnSIGHUP(runCtx)

	var runErr error
	select {
	case <-ctx.Done():
		d.logger.Printf("shutting down")
	case runErr = <-d.serveErr:
		d.logger.Printf("shutting down after error: %v", runErr)
	}
	// From here on no request may trigger a fresh startup: everything the
	// deferred stopAllProjects stops must stay stopped.
	d.draining.Store(true)
	cancelRun()

	// The control socket closes now; project listeners close in
	// stopAllProjects (deferred above), right before their processes stop.
	d.unbind(controlBinding)
	d.removeSocketFile()

	return runErr
}

// binding is one bound listener with its HTTP server. Project bindings
// serve through a handlerSwitch so a reload can replace the project behind
// the listener without rebinding it.
type binding struct {
	name     string // for log lines: "project <name>" or "control socket"
	listener net.Listener
	server   *http.Server
	sw       *handlerSwitch // nil for the control socket
}

// listenAddr returns the address a project's supervisor listener binds.
func listenAddr(p *config.Project) string {
	return net.JoinHostPort(p.ListenHost, strconv.Itoa(p.SupervisorPort))
}

// bindProject binds the listener for p and prepares — but does not start —
// its server, initially routing to h.
func (d *Daemon) bindProject(p *config.Project, h http.Handler) (*binding, error) {
	addr := listenAddr(p)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	sw := newHandlerSwitch(h)
	return &binding{
		name:     "project " + p.Name,
		listener: listener,
		server:   &http.Server{Handler: sw, ErrorLog: d.logger},
		sw:       sw,
	}, nil
}

// serve runs the binding's server until it is shut down. Any failure other
// than our own shutdown aborts the daemon.
func (d *Daemon) serve(b *binding) {
	if err := b.server.Serve(b.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		select {
		case d.serveErr <- fmt.Errorf("%s: serve: %w", b.name, err):
		default: // the first failure is the one that matters
		}
	}
}

// unbind stops accepting on the binding immediately and waits (bounded by
// shutdownTimeout) for its in-flight requests to finish.
func (d *Daemon) unbind(b *binding) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := b.server.Shutdown(ctx); err != nil {
		d.logger.Printf("%s: shutdown: %v", b.name, err)
	}
}

func (d *Daemon) logProxying(p *config.Project) {
	d.logger.Printf("project %q: proxying %s -> 127.0.0.1:%d (%s)", p.Name, listenAddr(p), p.ApplicationPort, p.PublicURL)
}

// handlerSwitch is the handler a project's listener serves: one atomic load
// per request to reach the project's current proxy handler. A reload that
// replaces the project on the same address suspends the switch — requests
// then wait (bounded only by their own context) until the replacement is
// installed — so the old process can be fully stopped before anything can
// cold-start the new configuration, and no request is refused meanwhile.
type handlerSwitch struct {
	slot atomic.Pointer[handlerSlot]
}

type handlerSlot struct {
	// handler is nil while the switch is suspended.
	handler http.Handler
	// ready is closed when a handler is installed after a suspension.
	ready chan struct{}
}

func newHandlerSwitch(h http.Handler) *handlerSwitch {
	s := &handlerSwitch{}
	s.slot.Store(&handlerSlot{handler: h})
	return s
}

func (s *handlerSwitch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for {
		slot := s.slot.Load()
		if slot.handler != nil {
			slot.handler.ServeHTTP(w, r)
			return
		}
		select {
		case <-slot.ready:
		case <-r.Context().Done():
			return // the client went away while waiting
		}
	}
}

// Suspend parks every new request until Install is called. A suspended
// switch stays suspended. Callers serialize Suspend/Install (reloadMu).
func (s *handlerSwitch) Suspend() {
	if s.slot.Load().handler == nil {
		return
	}
	s.slot.Store(&handlerSlot{ready: make(chan struct{})})
}

// Install routes requests to h from now on and releases any requests parked
// by a suspension.
func (s *handlerSwitch) Install(h http.Handler) {
	old := s.slot.Swap(&handlerSlot{handler: h})
	if old.handler == nil {
		close(old.ready)
	}
}

// activate starts the goroutine that drives the project's lifecycle for as
// long as the daemon runs: the idle monitor, or for always_on projects the
// immediate start. st.deactivate ends it (reload removing or replacing the
// project).
func (d *Daemon) activate(st *projectState, runCtx context.Context) {
	ctx, cancel := context.WithCancel(runCtx)
	st.deactivate = cancel
	if st.project.AlwaysOn {
		// always_on: start with the daemon (or the reload that added it),
		// never idle-stop. A failed start must not abort the daemon — the
		// project is marked failed and the usual retry paths (requests with
		// backoff, manual project:start) still apply.
		go func() {
			select {
			case err := <-st.proc.EnsureStarted():
				if err != nil {
					d.logger.Printf("project %q: always_on start failed: %v", st.project.Name, err)
				}
			case <-ctx.Done():
			}
		}()
		return
	}
	monitor := idle.NewMonitor(st.project.Name, st.proc, st.tracker, st.project.IdleTimeout(), d.logger)
	go monitor.Run(ctx)
}

// reloadOnSIGHUP reloads the configuration on every SIGHUP until ctx ends.
// It is the conventional daemon reload signal; the outcome is logged (the
// control API and `herd-wake reload` return it to the caller instead).
func (d *Daemon) reloadOnSIGHUP(ctx context.Context) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			d.logger.Printf("SIGHUP received: reloading config %s", d.configPath)
			resp, err := d.Reload(ctx)
			if err != nil {
				d.logger.Printf("reload: %v", err)
				continue
			}
			d.logReload(resp)
		}
	}
}

// claimSocket makes sure this daemon may take over the control socket path.
// If a daemon answers on the socket it returns an error; if the file exists
// but nothing accepts connections it removes the stale file.
func (d *Daemon) claimSocket(ctx context.Context) error {
	info, err := os.Lstat(d.socketPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect control socket %s: %w", d.socketPath, err)
	}
	if info.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("control socket path %s exists but is not a socket; move it out of the way and retry", d.socketPath)
	}

	conn, err := net.DialTimeout("unix", d.socketPath, time.Second)
	if err == nil {
		conn.Close() //nolint:errcheck // probe connection
		pingCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if status, err := control.NewClient(d.socketPath).Status(pingCtx); err == nil {
			return fmt.Errorf("another herd-wake daemon is already running (pid %d, up %s, control socket %s); stop it first",
				status.PID, status.Uptime().Round(time.Second), d.socketPath)
		}
		return fmt.Errorf("another process is already accepting connections on control socket %s; stop it first", d.socketPath)
	}

	d.logger.Printf("removing stale control socket %s (nothing is answering on it)", d.socketPath)
	if err := os.Remove(d.socketPath); err != nil {
		return fmt.Errorf("remove stale control socket %s: %w", d.socketPath, err)
	}
	return nil
}

// removeSocketFile deletes the control socket file if it still exists.
// (Closing the unix listener usually removes it already.)
func (d *Daemon) removeSocketFile() {
	if err := os.Remove(d.socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.logger.Printf("remove control socket %s: %v", d.socketPath, err)
	}
}
