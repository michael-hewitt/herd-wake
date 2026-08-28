// Command herd-wake is an on-demand lifecycle supervisor for Node.js
// development servers running alongside Laravel Herd.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/daemon"
	"github.com/michael-hewitt/herd-wake/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches the given command-line arguments and returns the process
// exit code. Output is written to stdout, diagnostics to stderr.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "herd-wake %s\n", version.String())
		return 0
	case "projects":
		return runProjects(args[1:], stdout, stderr)
	case "start":
		return runStart(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "project:start", "project:stop", "project:restart", "project:release":
		return runProjectCommand(args[0], args[1:], stdout, stderr)
	case "project:lease":
		return runProjectLease(args[1:], stdout, stderr)
	case "logs":
		return runLogs(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "herd-wake: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

// runProjects implements `herd-wake projects`: it loads the config and lists
// every registered project with its URL, ports, and command.
func runProjects(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("projects", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the config file (default: ~/Library/Application Support/herd-wake/config.yaml)")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	cfg, path, ok := loadConfig(*configPath, stderr)
	if !ok {
		return 1
	}

	if len(cfg.Projects) == 0 {
		fmt.Fprintf(stdout, "No projects configured in %s\n", path)
		return 0
	}

	for i, name := range cfg.ProjectNames() {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		printProject(stdout, cfg.Projects[name])
	}
	return 0
}

// runStart implements `herd-wake start`: it runs the supervisor daemon in
// the foreground until SIGINT or SIGTERM.
func runStart(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("start", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the config file (default: ~/Library/Application Support/herd-wake/config.yaml)")
	socketPath := flags.String("socket", "", "path to the control socket (default: ~/Library/Application Support/herd-wake/herd-wake.sock)")
	logDirFlag := flags.String("log-dir", "", "directory for per-project process logs (default: ~/Library/Application Support/herd-wake/logs)")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	cfg, _, ok := loadConfig(*configPath, stderr)
	if !ok {
		return 1
	}
	socket, ok := resolveSocketPath(*socketPath, stderr)
	if !ok {
		return 1
	}
	logDir := *logDirFlag
	if logDir == "" {
		var err error
		if logDir, err = config.LogsDir(); err != nil {
			fmt.Fprintf(stderr, "herd-wake: %v\n", err)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := log.New(stderr, "", log.LstdFlags)
	if err := daemon.New(cfg, socket, logDir, logger).Run(ctx); err != nil {
		fmt.Fprintf(stderr, "herd-wake: %v\n", err)
		return 1
	}
	return 0
}

// runStatus implements `herd-wake status`: it queries the daemon over the
// control socket and prints daemon uptime and per-project state.
func runStatus(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "path to the control socket (default: ~/Library/Application Support/herd-wake/herd-wake.sock)")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	socket, ok := resolveSocketPath(*socketPath, stderr)
	if !ok {
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := control.NewClient(socket).Status(ctx)
	if err != nil {
		reportDaemonError(stderr, err)
		return 1
	}

	fmt.Fprintf(stdout, "herd-wake daemon running: pid %d, uptime %s, version %s\n",
		status.PID, status.Uptime().Round(time.Second), status.Version)
	if len(status.Projects) == 0 {
		fmt.Fprintln(stdout, "No projects configured.")
		return 0
	}

	fmt.Fprintln(stdout)
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tSTATE\tPID\tUPTIME\tLAST ACTIVITY\tIDLE STOP\tLAST EXIT\tURL\tSUPERVISOR PORT\tUPSTREAM")
	for _, p := range status.Projects {
		pid, uptime, lastExit := "-", "-", "-"
		if p.PID != 0 {
			pid = strconv.Itoa(p.PID)
			uptime = (time.Duration(p.UptimeSeconds * float64(time.Second))).Round(time.Second).String()
		}
		if p.LastExit != "" {
			lastExit = p.LastExit
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t127.0.0.1:%d\n",
			p.Name, p.State, pid, uptime, describeLastActivity(p), describeIdleStop(p),
			lastExit, p.PublicURL, p.SupervisorPort, p.ApplicationPort)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "herd-wake: %v\n", err)
		return 1
	}
	for _, p := range status.Projects {
		if p.LastError != "" {
			fmt.Fprintf(stdout, "\n%s: %s\n", p.Name, p.LastError)
		}
	}
	return 0
}

// describeLastActivity renders the status column for when a project's most
// recent proxied request completed.
func describeLastActivity(p control.ProjectStatus) string {
	if p.LastActivityAt.IsZero() {
		return "-"
	}
	ago := time.Since(p.LastActivityAt).Round(time.Second)
	if ago < 0 {
		ago = 0
	}
	return ago.String() + " ago"
}

// describeIdleStop renders the status column for a project's pending idle
// stop: the scheduled time, what is holding it off, or why there is none.
func describeIdleStop(p control.ProjectStatus) string {
	switch {
	case p.State != daemon.StateRunning:
		return "-"
	case p.AlwaysOn:
		return "never (always on)"
	case p.InflightRequests > 0:
		return fmt.Sprintf("held (%d in flight)", p.InflightRequests)
	case !p.LeaseUntil.IsZero():
		return "leased until " + p.LeaseUntil.Local().Format("15:04:05")
	case !p.IdleStopAt.IsZero():
		in := time.Until(p.IdleStopAt).Round(time.Second)
		if in < 0 {
			in = 0
		}
		return fmt.Sprintf("in %s (%s)", in, p.IdleStopAt.Local().Format("15:04:05"))
	default:
		return "-"
	}
}

// runProjectLease implements `herd-wake project:lease --ttl <duration>
// <name>`: it marks the project active for the given duration so external
// tools can hold off idle shutdown without generating traffic.
func runProjectLease(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("project:lease", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "path to the control socket (default: ~/Library/Application Support/herd-wake/herd-wake.sock)")
	ttl := flags.Duration("ttl", 30*time.Minute, "how long the lease lasts, e.g. 30m or 2h")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || flags.Arg(0) == "" {
		fmt.Fprintf(stderr, "herd-wake: usage: herd-wake project:lease [--ttl duration] <name>\n")
		return 2
	}
	if *ttl <= 0 {
		fmt.Fprintf(stderr, "herd-wake: --ttl must be positive (got %s)\n", *ttl)
		return 2
	}
	name := flags.Arg(0)

	socket, ok := resolveSocketPath(*socketPath, stderr)
	if !ok {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := control.NewClient(socket).LeaseProject(ctx, name, *ttl)
	if err != nil {
		reportDaemonError(stderr, err)
		return 1
	}

	fmt.Fprintf(stdout, "Project %q leased until %s (ttl %s); it will not be idle-stopped before then.\n",
		name, status.LeaseUntil.Local().Format("15:04:05"), *ttl)
	fmt.Fprintf(stdout, "Release early with: herd-wake project:release %s\n", name)
	return 0
}

// runProjectCommand implements `herd-wake project:start|project:stop|
// project:restart|project:release <name>` as control-client calls against
// the daemon.
func runProjectCommand(command string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "path to the control socket (default: ~/Library/Application Support/herd-wake/herd-wake.sock)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || flags.Arg(0) == "" {
		fmt.Fprintf(stderr, "herd-wake: usage: herd-wake %s <name>\n", command)
		return 2
	}
	name := flags.Arg(0)

	socket, ok := resolveSocketPath(*socketPath, stderr)
	if !ok {
		return 1
	}
	client := control.NewClient(socket)

	// Generous bound: starting waits for readiness (startup_timeout_seconds),
	// stopping waits for the graceful shutdown timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var status *control.ProjectStatus
	var err error
	switch command {
	case "project:start":
		status, err = client.StartProject(ctx, name)
	case "project:stop":
		status, err = client.StopProject(ctx, name)
	case "project:restart":
		status, err = client.RestartProject(ctx, name)
	case "project:release":
		status, err = client.ReleaseProjectLease(ctx, name)
	}
	if err != nil {
		reportDaemonError(stderr, err)
		if (command == "project:start" || command == "project:restart") && !errors.Is(err, control.ErrDaemonUnreachable) {
			fmt.Fprintf(stderr, "See recent output with: herd-wake logs %s\n", name)
		}
		return 1
	}

	if command == "project:release" {
		fmt.Fprintf(stdout, "Released activity lease for project %q; normal idle rules apply again.\n", name)
		return 0
	}
	if status.State == daemon.StateRunning {
		fmt.Fprintf(stdout, "Project %q is running (pid %d).\n", name, status.PID)
	} else {
		fmt.Fprintf(stdout, "Project %q is %s.\n", name, status.State)
	}
	return 0
}

// runLogs implements `herd-wake logs <name>`: it prints the project's recent
// combined stdout/stderr captured by the daemon.
func runLogs(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "path to the control socket (default: ~/Library/Application Support/herd-wake/herd-wake.sock)")
	lines := flags.Int("lines", 0, "maximum lines to print (0 = everything buffered, up to 200)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || flags.Arg(0) == "" {
		fmt.Fprintf(stderr, "herd-wake: usage: herd-wake logs [--lines n] <name>\n")
		return 2
	}
	name := flags.Arg(0)

	socket, ok := resolveSocketPath(*socketPath, stderr)
	if !ok {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logs, err := control.NewClient(socket).Logs(ctx, name, *lines)
	if err != nil {
		reportDaemonError(stderr, err)
		return 1
	}

	for _, line := range logs.Lines {
		fmt.Fprintln(stdout, line)
	}
	if len(logs.Lines) == 0 {
		fmt.Fprintf(stderr, "No recent output captured for project %q.\n", name)
	}
	fmt.Fprintf(stderr, "Full log: %s\n", logs.LogFile)
	return 0
}

// reportDaemonError prints a control-client error: a friendly hint when the
// daemon is not answering on its socket, the daemon's message otherwise.
func reportDaemonError(stderr io.Writer, err error) {
	if errors.Is(err, control.ErrDaemonUnreachable) {
		fmt.Fprintf(stderr, "herd-wake: daemon is not running (%v)\n", err)
		fmt.Fprintf(stderr, "Start it with: herd-wake start\n")
		return
	}
	fmt.Fprintf(stderr, "herd-wake: %v\n", err)
}

// loadConfig resolves the config path (flag value or default location) and
// loads it, reporting problems to stderr. The bool result is false when the
// caller should exit with an error.
func loadConfig(flagPath string, stderr io.Writer) (cfg *config.Config, path string, ok bool) {
	path = flagPath
	if path == "" {
		defaultPath, err := config.DefaultPath()
		if err != nil {
			fmt.Fprintf(stderr, "herd-wake: %v\n", err)
			return nil, "", false
		}
		path = defaultPath
	}

	cfg, err := config.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		printMissingConfig(stderr, path)
		return nil, path, false
	}
	if err != nil {
		fmt.Fprintf(stderr, "herd-wake: config %s is invalid:\n", path)
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(stderr, "  - %s\n", line)
		}
		return nil, path, false
	}
	return cfg, path, true
}

// resolveSocketPath returns the control socket path: the flag value if set,
// otherwise the default under the herd-wake base directory.
func resolveSocketPath(flagPath string, stderr io.Writer) (string, bool) {
	if flagPath != "" {
		return flagPath, true
	}
	socket, err := config.SocketPath()
	if err != nil {
		fmt.Fprintf(stderr, "herd-wake: %v\n", err)
		return "", false
	}
	return socket, true
}

// printProject writes one project's registration in a human-readable block.
func printProject(w io.Writer, p *config.Project) {
	name := p.Name
	if p.AlwaysOn {
		name += "  (always on)"
	}
	fmt.Fprintln(w, name)
	fmt.Fprintf(w, "  URL:               %s\n", p.PublicURL)
	fmt.Fprintf(w, "  Supervisor port:   %d  (herd proxy target %s:%d)\n", p.SupervisorPort, p.ListenHost, p.SupervisorPort)
	fmt.Fprintf(w, "  Application port:  %d\n", p.ApplicationPort)
	fmt.Fprintf(w, "  Working directory: %s\n", p.WorkingDirectory)
	fmt.Fprintf(w, "  Command:           %s\n", p.Command)
	fmt.Fprintf(w, "  Readiness:         %s", p.ReadinessStrategy)
	if p.ReadinessStrategy == config.ReadinessHTTP {
		fmt.Fprintf(w, " %s", p.ReadinessURL)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Timeouts:          startup %ds, idle %dm\n", p.StartupTimeoutSeconds, p.IdleTimeoutMinutes)
}

// printMissingConfig tells the user where the config file belongs and shows a
// minimal example to start from.
func printMissingConfig(w io.Writer, path string) {
	fmt.Fprintf(w, `herd-wake: no config file found at %s

Create one there to register your projects. A minimal example:

  projects:
    dashboard:
      public_url: https://dashboard.test
      supervisor_port: 7101
      application_port: 17101
      working_directory: /Users/you/Code/dashboard
      command: npm run dev -- --port 17101 --strictPort

A fully documented example ships with herd-wake as config.sample.yaml:
https://github.com/michael-hewitt/herd-wake/blob/main/config.sample.yaml
`, path)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: herd-wake <command>

Commands:
  start                    Run the supervisor daemon in the foreground (Ctrl-C to stop)
  status                   Show daemon uptime and per-project state
  projects                 List registered projects from the config file
  project:start <name>     Start a project's dev server and wait until it is ready
  project:stop <name>      Gracefully stop a project's dev server
  project:restart <name>   Stop (if needed) and start a project's dev server
  project:lease <name>     Mark a project active so it is not idle-stopped (--ttl, default 30m)
  project:release <name>   Release a project's activity lease early
  logs <name>              Print a project's recent dev-server output
  version                  Print the herd-wake version

Options:
  --config <path>   Config file to load (start, projects)
                    (default: ~/Library/Application Support/herd-wake/config.yaml)
  --socket <path>   Control socket to use (start, status, project:*, logs)
                    (default: ~/Library/Application Support/herd-wake/herd-wake.sock)
  --log-dir <path>  Directory for per-project process logs (start)
                    (default: ~/Library/Application Support/herd-wake/logs)
  --ttl <duration>  How long a lease lasts, e.g. 45m or 2h (project:lease; default 30m)
  --lines <n>       Maximum lines to print (logs; 0 = everything buffered)
`)
}
