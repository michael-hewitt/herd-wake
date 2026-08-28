// Package testproc turns a Go test binary into the child processes spawned
// by herd-wake's process-lifecycle tests: a TCP listener, an HTTP server, a
// signal-ignoring process, a parent that spawns a listening child, and a
// listener that exits cleanly on its own. Tests call Main from TestMain so
// re-invoking the test binary with HW_TESTPROC_MODE set runs a helper
// instead of the test suite — no external tools (nc, node) needed.
//
// Production code never imports this package; only _test files do, so none
// of it is linked into the herd-wake binary.
package testproc

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// Environment variables controlling a helper process.
const (
	// EnvMode selects the helper mode; unset means "run the tests normally".
	EnvMode = "HW_TESTPROC_MODE"
	// EnvPort is the loopback TCP port listener modes bind.
	EnvPort = "HW_TESTPROC_PORT"
	// EnvPidfile, when set, receives this helper's pid on startup.
	EnvPidfile = "HW_TESTPROC_PIDFILE"
	// EnvChildPidfile is where mode "parent" tells its child to write its pid.
	EnvChildPidfile = "HW_TESTPROC_CHILD_PIDFILE"
	// EnvChildMode selects which mode "parent" spawns its child in
	// (default ModeListen). ModeStubborn reproduces the shell-topology bug:
	// a group-wide SIGTERM kills the parent instantly while the stubborn
	// child survives, so only a full group drain plus SIGKILL ends the group.
	EnvChildMode = "HW_TESTPROC_CHILD_MODE"
	// EnvLinger is how long mode "listen-exit" stays alive (Go duration,
	// default 300ms).
	EnvLinger = "HW_TESTPROC_LINGER"
	// EnvStartDelay, when set (Go duration), makes the listener modes sleep
	// before binding their port — a slow-starting server for cold-start and
	// request-holding tests.
	EnvStartDelay = "HW_TESTPROC_START_DELAY"
)

// Helper modes.
const (
	// ModeListen binds a TCP listener and exits 0 on SIGTERM/SIGINT.
	ModeListen = "listen"
	// ModeHTTP serves 200 OK on the port (echoing method, path, and Host so
	// proxy tests can assert what arrived) and exits 0 on SIGTERM/SIGINT.
	// A request with a ?sleep=<duration> query sleeps that long before
	// answering, so tests can keep a request in flight deliberately.
	ModeHTTP = "http"
	// ModeHTTPStubborn is ModeHTTP but ignoring SIGTERM/SIGINT: only
	// SIGKILL ends it, so a graceful stop reliably takes the full shutdown
	// timeout — a wide window for request-during-stop tests.
	ModeHTTPStubborn = "http-stubborn"
	// ModeWS serves a minimal WebSocket echo server on the port (see
	// WSEchoHandler; non-upgrade requests get the plain HTTP echo) and
	// exits 0 on SIGTERM/SIGINT.
	ModeWS = "ws"
	// ModeStubborn binds a TCP listener and ignores SIGTERM/SIGINT; only
	// SIGKILL ends it.
	ModeStubborn = "stubborn"
	// ModeParent spawns this binary again as a child (mode from EnvChildMode,
	// default ModeListen; child pidfile from EnvChildPidfile) and waits on
	// it, so the process group has real children.
	ModeParent = "parent"
	// ModeListenExit binds a TCP listener, lingers briefly, then exits 0 on
	// its own.
	ModeListenExit = "listen-exit"
)

// Main runs the requested helper mode and exits the process, or returns
// immediately when EnvMode is unset. Call it first thing in TestMain.
func Main() {
	mode := os.Getenv(EnvMode)
	if mode == "" {
		return
	}
	os.Exit(run(mode))
}

// Command returns the shell command that re-invokes the current binary (the
// test binary) as a helper. Pair it with env entries for EnvMode and
// friends on the project config.
func Command() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve test binary path: %w", err)
	}
	return "'" + exe + "'", nil
}

func run(mode string) int {
	switch mode {
	case ModeListen:
		return listen()
	case ModeHTTP:
		return serveHTTP(false)
	case ModeHTTPStubborn:
		return serveHTTP(true)
	case ModeWS:
		return serveWS()
	case ModeStubborn:
		return stubborn()
	case ModeParent:
		return parent()
	case ModeListenExit:
		return listenExit()
	default:
		fmt.Fprintf(os.Stderr, "testproc: unknown mode %q\n", mode)
		return 2
	}
}

func listen() int {
	writePidfile()
	startDelay()
	ln, err := bindPort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testproc:", err)
		return 1
	}
	fmt.Printf("testproc listening on %s\n", ln.Addr())
	go acceptLoop(ln)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	<-sigs
	fmt.Println("testproc exiting on signal")
	return 0
}

func serveHTTP(stubborn bool) int {
	writePidfile()
	if stubborn {
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
	}
	startDelay()
	ln, err := bindPort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testproc:", err)
		return 1
	}
	fmt.Printf("testproc serving http on %s\n", ln.Addr())
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.URL.Query().Get("sleep"); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				time.Sleep(d)
			}
		}
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "ok %s %s host=%s body=%s pid=%d\n", r.Method, r.URL.Path, r.Host, body, os.Getpid())
	})}
	if stubborn {
		if err := server.Serve(ln); err != nil {
			fmt.Fprintln(os.Stderr, "testproc:", err)
			return 1
		}
		return 0 // unreachable: only SIGKILL ends this process
	}
	go server.Serve(ln) //nolint:errcheck // process exits on signal below

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	<-sigs
	fmt.Println("testproc exiting on signal")
	return 0
}

// serveHandler binds the port, serves handler on it, and exits 0 on
// SIGTERM/SIGINT. banner is printed (with the listen address) once bound.
func serveHandler(handler http.Handler, banner string) int {
	writePidfile()
	startDelay()
	ln, err := bindPort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testproc:", err)
		return 1
	}
	fmt.Printf(banner, ln.Addr())
	server := &http.Server{Handler: handler}
	go server.Serve(ln) //nolint:errcheck // process exits on signal below

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	<-sigs
	fmt.Println("testproc exiting on signal")
	return 0
}

func stubborn() int {
	writePidfile()
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
	ln, err := bindPort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testproc:", err)
		return 1
	}
	fmt.Printf("testproc stubbornly listening on %s\n", ln.Addr())
	acceptLoop(ln) // never returns; only SIGKILL ends this process
	return 0
}

func parent() int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testproc:", err)
		return 1
	}
	childMode := os.Getenv(EnvChildMode)
	if childMode == "" {
		childMode = ModeListen
	}
	child := exec.Command(exe)
	child.Env = append(os.Environ(),
		EnvMode+"="+childMode,
		EnvPidfile+"="+os.Getenv(EnvChildPidfile),
	)
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "testproc: start child:", err)
		return 1
	}
	writePidfile()
	fmt.Printf("testproc parent %d spawned child %d\n", os.Getpid(), child.Process.Pid)
	// No signal handler: a group-wide SIGTERM terminates this process (and,
	// delivered to the whole group, the child too).
	if err := child.Wait(); err != nil {
		return 1
	}
	return 0
}

func listenExit() int {
	writePidfile()
	ln, err := bindPort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testproc:", err)
		return 1
	}
	fmt.Printf("testproc listening on %s\n", ln.Addr())
	go acceptLoop(ln)

	linger := 300 * time.Millisecond
	if v := os.Getenv(EnvLinger); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			linger = d
		}
	}
	time.Sleep(linger)
	fmt.Println("testproc exiting cleanly")
	return 0
}

func bindPort() (net.Listener, error) {
	port, err := strconv.Atoi(os.Getenv(EnvPort))
	if err != nil {
		return nil, fmt.Errorf("bad %s: %w", EnvPort, err)
	}
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

func acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Close() //nolint:errcheck // connect-probe peer; nothing to say
	}
}

func writePidfile() {
	if path := os.Getenv(EnvPidfile); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
	}
}

// startDelay sleeps for EnvStartDelay (if set) to simulate a server that
// takes a while to become ready.
func startDelay() {
	if v := os.Getenv(EnvStartDelay); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			time.Sleep(d)
		}
	}
}
