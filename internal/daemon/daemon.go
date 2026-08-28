// Package daemon wires the herd-wake supervisor daemon together: one
// loopback reverse-proxy listener per registered project, a process
// supervisor per project, and the control API on a unix socket. It owns
// listener lifecycle — binding, serving, and clean shutdown including
// control-socket file removal — and guarantees every process group it
// started is terminated before the daemon exits.
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
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/proxy"
)

// shutdownTimeout bounds how long Run waits for in-flight requests when
// shutting down.
const shutdownTimeout = 5 * time.Second

// Daemon is the supervisor daemon: per-project proxy listeners, per-project
// process supervisors, and the control socket.
type Daemon struct {
	cfg        *config.Config
	socketPath string
	logger     *log.Logger
	states     []*projectState
	startedAt  time.Time
}

// New builds a daemon for the given configuration. The control API listens
// on the unix socket at socketPath; project process output is written under
// logDir (one <name>.log per project); diagnostics go to logger.
func New(cfg *config.Config, socketPath, logDir string, logger *log.Logger) *Daemon {
	return &Daemon{
		cfg:        cfg,
		socketPath: socketPath,
		logger:     logger,
		states:     newProjectStates(cfg, logDir, logger),
	}
}

// Run binds every listener and serves until ctx is cancelled (SIGINT/SIGTERM
// in the CLI) or a listener fails. On return all listeners are closed and
// the control socket file is removed.
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

	type boundServer struct {
		name     string
		listener net.Listener
		server   *http.Server
	}
	servers := []boundServer{{
		name:     "control socket",
		listener: controlListener,
		server:   &http.Server{Handler: control.NewHandler(d), ErrorLog: d.logger},
	}}
	closeAll := func() {
		for _, s := range servers {
			s.listener.Close() //nolint:errcheck // best-effort cleanup
		}
		d.removeSocketFile()
	}

	for _, st := range d.states {
		p := st.project
		addr := net.JoinHostPort(p.ListenHost, strconv.Itoa(p.SupervisorPort))
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			closeAll()
			return fmt.Errorf("project %q: listen on %s: %w", p.Name, addr, err)
		}
		servers = append(servers, boundServer{
			name:     "project " + p.Name,
			listener: listener,
			server:   &http.Server{Handler: proxy.NewOnDemand(p, st.proc, d.logger), ErrorLog: d.logger},
		})
		d.logger.Printf("project %q: proxying %s -> 127.0.0.1:%d (%s)", p.Name, addr, p.ApplicationPort, p.PublicURL)
	}

	d.startedAt = time.Now()
	d.logger.Printf("daemon ready: %d project(s), control socket %s", len(d.states), d.socketPath)

	// Serve everything; the first real failure (not ErrServerClosed from our
	// own shutdown) aborts the daemon.
	serveErr := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.server.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- fmt.Errorf("%s: serve: %w", s.name, err)
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		d.logger.Printf("shutting down")
	case runErr = <-serveErr:
		d.logger.Printf("shutting down after error: %v", runErr)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.server.Shutdown(shutdownCtx); err != nil {
				d.logger.Printf("%s: shutdown: %v", s.name, err)
			}
		}()
	}
	wg.Wait()
	d.removeSocketFile()

	return runErr
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
