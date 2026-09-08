package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/control"
	"github.com/michael-hewitt/herd-wake/internal/discovery"
	"github.com/michael-hewitt/herd-wake/internal/herd"
)

// reloadOutcome records the daemon reload sync/project:remove trigger
// after changing the configuration.
type reloadOutcome struct {
	// Attempted is false on a dry run.
	Attempted bool `json:"attempted"`
	// DaemonRunning is false when nothing answered on the control socket.
	DaemonRunning bool `json:"daemon_running"`
	// Error is a failure to perform the reload (not a rejection).
	Error string `json:"error,omitempty"`
	// Response is the daemon's answer, when it answered.
	Response *control.ReloadResponse `json:"response,omitempty"`
}

// failed reports whether the reload went wrong in a way that should fail
// the command: an error talking to a running daemon, a rejected reload, or
// per-project apply errors. A daemon that is not running is not a failure.
func (r *reloadOutcome) failed() bool {
	if !r.Attempted || !r.DaemonRunning {
		return false
	}
	if r.Error != "" {
		return true
	}
	return r.Response != nil && (!r.Response.Applied || len(r.Response.Errors) > 0)
}

// syncOutput is the JSON document `herd-wake sync --json` prints.
type syncOutput struct {
	*discovery.Result
	Reload reloadOutcome `json:"reload"`
}

// runSync implements `herd-wake sync`: it reconciles every discovery entry
// with the filesystem and Herd, then reloads the daemon if it is running.
func runSync(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the config file (default: ~/Library/Application Support/herd-wake/config.yaml)")
	socketPath := flags.String("socket", "", "path to the control socket (default: ~/Library/Application Support/herd-wake/herd-wake.sock)")
	dryRun := flags.Bool("dry-run", false, "compute and print the changes without writing files or touching Herd")
	noHerd := flags.Bool("no-herd", false, "do not run the herd CLI at all; print the Herd commands to run by hand")
	asJSON := flags.Bool("json", false, "print the result as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "herd-wake: usage: herd-wake sync [--config path] [--socket path] [--dry-run] [--no-herd] [--json]\n")
		return 2
	}

	cfg, path, ok := loadConfigWithOptions(*configPath, config.LoadOptions{SkipWorkingDirectoryCheck: true}, stderr)
	if !ok {
		return 1
	}
	if len(cfg.Discovery) == 0 {
		fmt.Fprintf(stderr, "herd-wake: no discovery entries in %s; add a discovery: list to scan a directory of worktrees (see config.sample.yaml)\n", path)
		return 1
	}
	socket, ok := resolveSocketPath(*socketPath, stderr)
	if !ok {
		return 1
	}

	ctx := context.Background()
	opts := discovery.Options{DryRun: *dryRun}
	opts.Herd, opts.HerdNote = detectHerd(*noHerd)
	result, err := discovery.Sync(ctx, cfg, opts)
	if err != nil {
		fmt.Fprintf(stderr, "herd-wake: %v\n", err)
		return 1
	}

	out := syncOutput{Result: result}
	if !*dryRun {
		out.Reload = triggerReload(ctx, socket)
	}

	if *asJSON {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "herd-wake: %v\n", err)
			return 1
		}
	} else {
		printSync(stdout, out)
	}
	if result.HasErrors() || out.Reload.failed() {
		return 1
	}
	return 0
}

// runProjectRemove implements `herd-wake project:remove <name>`: it deletes
// the project from its projects.d file, removes its Herd proxy, and reloads
// the daemon.
func runProjectRemove(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("project:remove", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the config file (default: ~/Library/Application Support/herd-wake/config.yaml)")
	socketPath := flags.String("socket", "", "path to the control socket (default: ~/Library/Application Support/herd-wake/herd-wake.sock)")
	keepHerd := flags.Bool("keep-herd", false, "leave the project's Herd proxy in place")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || flags.Arg(0) == "" {
		fmt.Fprintf(stderr, "herd-wake: usage: herd-wake project:remove [--config path] [--socket path] [--keep-herd] <name>\n")
		return 2
	}
	name := flags.Arg(0)

	cfg, path, ok := loadConfigWithOptions(*configPath, config.LoadOptions{SkipWorkingDirectoryCheck: true}, stderr)
	if !ok {
		return 1
	}
	socket, ok := resolveSocketPath(*socketPath, stderr)
	if !ok {
		return 1
	}

	ctx := context.Background()
	opts := discovery.RemoveOptions{KeepHerd: *keepHerd}
	if !*keepHerd {
		opts.Herd, opts.HerdNote = detectHerd(false)
	}
	res, err := discovery.Remove(ctx, cfg, name, opts)
	var mainErr *discovery.MainConfigError
	switch {
	case errors.Is(err, discovery.ErrNoSuchProject):
		fmt.Fprintf(stderr, "herd-wake: no project named %q in %s or %s/; nothing to remove\n", name, path, config.ProjectsDir(path))
		return 0
	case errors.As(err, &mainErr):
		fmt.Fprintf(stderr, "herd-wake: %v.\n", err)
		fmt.Fprintf(stderr, "After removing it, run:\n  %s\n  herd-wake reload\n", herdCommandForProject(cfg.Projects[name]))
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "herd-wake: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Removed project %q from %s\n", name, res.File)
	if res.Managed {
		fmt.Fprintf(stdout, "  (a discovery-managed file: the next `herd-wake sync` re-creates the project while its directory exists)\n")
	}
	for _, a := range res.Herd {
		fmt.Fprintf(stdout, "  herd: %s\n", describeHerdAction(a))
	}
	reload := triggerReload(ctx, socket)
	printReload(stdout, reload)
	if reload.failed() {
		return 1
	}
	return 0
}

// detectHerd resolves the Herd CLI for sync/remove, or explains why it is
// not used. disabled reflects --no-herd.
func detectHerd(disabled bool) (*herd.CLI, string) {
	if disabled {
		return nil, "--no-herd: Herd left untouched"
	}
	cli, err := herd.Detect(herd.Options{})
	if err != nil {
		return nil, err.Error()
	}
	return cli, ""
}

// triggerReload asks a running daemon to reload; a daemon that is not
// running is reported, not treated as an error.
func triggerReload(ctx context.Context, socket string) reloadOutcome {
	out := reloadOutcome{Attempted: true}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	resp, err := control.NewClient(socket).Reload(ctx)
	switch {
	case errors.Is(err, control.ErrDaemonUnreachable):
		return out
	case err != nil:
		out.DaemonRunning = true
		out.Error = err.Error()
		return out
	}
	out.DaemonRunning = true
	out.Response = resp
	return out
}

// printSync renders a sync result for humans.
func printSync(w io.Writer, out syncOutput) {
	if out.DryRun {
		fmt.Fprintln(w, "Dry run: nothing was written and Herd was not changed.")
	}
	for _, e := range out.Entries {
		fmt.Fprintf(w, "Discovery %q (%s) → %s\n", e.Name, e.Directory, e.File)
		for _, err := range e.Errors {
			fmt.Fprintf(w, "  error:     %s\n", err)
		}
		printSummaries(w, "added", e.Added)
		printSummaries(w, "updated", e.Updated)
		printSummaries(w, "unchanged", e.Unchanged)
		printSummaries(w, "removed", e.Removed)
		for i, s := range e.Skipped {
			fmt.Fprintf(w, "  %-10s %s — %s\n", labelFor("skipped", i), s.Name, s.Reason)
		}
		for i, a := range e.Herd {
			fmt.Fprintf(w, "  %-10s %s\n", labelFor("herd", i), describeHerdAction(a))
		}
		switch {
		case len(e.Errors) > 0:
		case out.DryRun && e.Changed:
			fmt.Fprintf(w, "  file:      would be rewritten\n")
		case out.DryRun:
			fmt.Fprintf(w, "  file:      up to date\n")
		case e.Written:
			fmt.Fprintf(w, "  file:      written\n")
		default:
			fmt.Fprintf(w, "  file:      up to date\n")
		}
	}
	if out.HerdNote != "" {
		fmt.Fprintf(w, "Herd: %s\n", out.HerdNote)
	}
	if manual := out.ManualCommands(); len(manual) > 0 {
		fmt.Fprintln(w, "Run these Herd commands yourself:")
		for _, cmd := range manual {
			fmt.Fprintf(w, "  %s\n", cmd)
		}
	}
	if !out.DryRun {
		printReload(w, out.Reload)
	}
}

// printSummaries prints one group of a sync result.
func printSummaries(w io.Writer, label string, list []discovery.ProjectSummary) {
	if len(list) == 0 {
		return
	}
	for i, p := range list {
		line := fmt.Sprintf("%s (%s, supervisor %d, application %d)", p.Name, p.PublicURL, p.SupervisorPort, p.ApplicationPort)
		if p.Detail != "" {
			line += ": " + p.Detail
		}
		fmt.Fprintf(w, "  %-10s %s\n", labelFor(label, i), line)
	}
}

// labelFor renders a group label on its first line and blanks after.
func labelFor(label string, i int) string {
	if i == 0 {
		return label + ":"
	}
	return ""
}

// describeHerdAction renders one Herd action.
func describeHerdAction(a discovery.HerdAction) string {
	var s string
	switch a.Action {
	case "proxy":
		s = fmt.Sprintf("proxy %s → http://127.0.0.1:%d", a.Site, a.Port)
	default:
		s = fmt.Sprintf("%s %s", a.Action, a.Site)
	}
	switch a.Status {
	case discovery.HerdDone:
		s += " (done"
	case discovery.HerdDryRun:
		s += " (would run: " + a.Command
	case discovery.HerdSkipped:
		s += " (skipped"
	case discovery.HerdManual:
		s += " (run by hand: " + a.Command
	default:
		s += " (" + a.Status
	}
	if a.Detail != "" {
		s += "; " + a.Detail
	}
	return s + ")"
}

// printReload renders the reload outcome.
func printReload(w io.Writer, r reloadOutcome) {
	switch {
	case !r.Attempted:
		return
	case !r.DaemonRunning:
		fmt.Fprintln(w, "Daemon: not running; it reads the configuration when started (or run `herd-wake reload` later).")
	case r.Error != "":
		fmt.Fprintf(w, "Daemon: reload failed: %s\n", r.Error)
	case !r.Response.Applied:
		fmt.Fprintf(w, "Daemon: reload rejected, config %s is invalid:\n", r.Response.ConfigPath)
		for _, line := range r.Response.Errors {
			fmt.Fprintf(w, "  - %s\n", line)
		}
		fmt.Fprintln(w, "Nothing was changed in the daemon; it keeps running with its previous configuration.")
	default:
		fmt.Fprintf(w, "Daemon: reloaded — added %s, removed %s, changed %s, unchanged %s\n",
			nameList(r.Response.Added), nameList(r.Response.Removed), nameList(r.Response.Changed), nameList(r.Response.Unchanged))
		for _, line := range r.Response.Errors {
			fmt.Fprintf(w, "  - %s\n", line)
		}
	}
}

func nameList(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// herdCommandForProject renders the unproxy command for a project's site.
func herdCommandForProject(p *config.Project) string {
	if p == nil {
		return "herd unproxy <name>"
	}
	if _, site, ok := herd.SiteFromURL(p.PublicURL); ok {
		return herd.UnproxyCommand(site)
	}
	return "herd unproxy <name>"
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// loadConfigWithOptions is loadConfig with load options.
func loadConfigWithOptions(flagPath string, opts config.LoadOptions, stderr io.Writer) (cfg *config.Config, path string, ok bool) {
	path = flagPath
	if path == "" {
		defaultPath, err := config.DefaultPath()
		if err != nil {
			fmt.Fprintf(stderr, "herd-wake: %v\n", err)
			return nil, "", false
		}
		path = defaultPath
	}

	cfg, err := config.LoadWithOptions(path, opts)
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
