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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := log.New(stderr, "", log.LstdFlags)
	if err := daemon.New(cfg, socket, logger).Run(ctx); err != nil {
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
		fmt.Fprintf(stderr, "herd-wake: daemon is not running (%v)\n", err)
		fmt.Fprintf(stderr, "Start it with: herd-wake start\n")
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
	fmt.Fprintln(tw, "PROJECT\tSTATE\tURL\tSUPERVISOR PORT\tUPSTREAM")
	for _, p := range status.Projects {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t127.0.0.1:%d\n",
			p.Name, p.State, p.PublicURL, p.SupervisorPort, p.ApplicationPort)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "herd-wake: %v\n", err)
		return 1
	}
	return 0
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
  start      Run the supervisor daemon in the foreground (Ctrl-C to stop)
  status     Show daemon uptime and per-project state
  projects   List registered projects from the config file
  version    Print the herd-wake version

Options:
  --config <path>   Config file to load (start, projects)
                    (default: ~/Library/Application Support/herd-wake/config.yaml)
  --socket <path>   Control socket to use (start, status)
                    (default: ~/Library/Application Support/herd-wake/herd-wake.sock)
`)
}
