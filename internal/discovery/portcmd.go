package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
)

// runPortCommand runs a discovery entry's port_command in the worktree at
// dir (via /bin/sh -c, with the template's env and node_path applied like
// the project's own command gets them) and parses the port it prints.
func runPortCommand(ctx context.Context, dir, command string, template *config.Project, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = commandEnv(template)
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return 0, fmt.Errorf("%q timed out after %s", command, timeout)
	}
	if err != nil {
		return 0, fmt.Errorf("%q: %w%s", command, err, outputHint(stderr.String(), stdout.String()))
	}
	port, err := parsePort(stdout.String())
	if err != nil {
		return 0, fmt.Errorf("%q: %v", command, err)
	}
	return port, nil
}

// commandEnv mirrors the environment a project's command gets: the
// current environment, PATH with node_path prepended, and the template's
// env entries (later entries win).
func commandEnv(template *config.Project) []string {
	env := os.Environ()
	if template.NodePath != "" {
		dir := template.NodePath
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			dir = filepath.Dir(dir)
		}
		env = append(env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	keys := make([]string, 0, len(template.Env))
	for k := range template.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+template.Env[k])
	}
	return env
}

// outputHint appends the command's output (stderr first) to an error.
func outputHint(stderr, stdout string) string {
	out := strings.TrimSpace(stderr)
	if out == "" {
		out = strings.TrimSpace(stdout)
	}
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	if len(lines) > 5 {
		lines = append([]string{"…"}, lines[len(lines)-5:]...)
	}
	return ": " + strings.Join(lines, " / ")
}

var trailingInteger = regexp.MustCompile(`(\d+)\s*$`)

// parsePort extracts the application port from port_command output: the
// last non-empty line as a bare integer, or a URL (its explicit port, or
// 80/443 by scheme), or failing both, a trailing integer anywhere in the
// output.
func parsePort(output string) (int, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return 0, errors.New("printed nothing; expected a port number or a URL")
	}
	lines := strings.Split(trimmed, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])

	if port, err := strconv.Atoi(last); err == nil {
		return checkPort(port, last)
	}
	if u, err := url.Parse(last); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		switch p := u.Port(); {
		case p != "":
			port, err := strconv.Atoi(p)
			if err != nil {
				return 0, fmt.Errorf("URL %q has a non-numeric port", last)
			}
			return checkPort(port, last)
		case u.Scheme == "https":
			return 443, nil
		default:
			return 80, nil
		}
	}
	if m := trailingInteger.FindStringSubmatch(trimmed); m != nil {
		port, err := strconv.Atoi(m[1])
		if err == nil {
			return checkPort(port, last)
		}
	}
	return 0, fmt.Errorf("could not find a port in its output (last line: %q); expected a port number or a URL", last)
}

func checkPort(port int, from string) (int, error) {
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("port %d (from %q) is out of range 1–65535", port, from)
	}
	return port, nil
}
