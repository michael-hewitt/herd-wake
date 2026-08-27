// Command herd-wake is an on-demand lifecycle supervisor for Node.js
// development servers running alongside Laravel Herd.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/michael-hewitt/herd-wake/internal/config"
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

	path := *configPath
	if path == "" {
		defaultPath, err := config.DefaultPath()
		if err != nil {
			fmt.Fprintf(stderr, "herd-wake: %v\n", err)
			return 1
		}
		path = defaultPath
	}

	cfg, err := config.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		printMissingConfig(stderr, path)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "herd-wake: config %s is invalid:\n", path)
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(stderr, "  - %s\n", line)
		}
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
  projects   List registered projects from the config file
  version    Print the herd-wake version

Options for projects:
  --config <path>   Config file to load
                    (default: ~/Library/Application Support/herd-wake/config.yaml)
`)
}
