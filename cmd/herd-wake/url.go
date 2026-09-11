package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/discovery"
)

// runURL implements `herd-wake url [directory]`: it prints the public URL
// the directory (default: the current one) is served at, so repo hooks and
// scripts need not know the domain scheme. A registered project's
// working_directory answers with its public_url; a worktree under a
// wildcard discovery entry's directory answers with
// https://<label>.<base_domain> when it passes the entry's rules; a
// worktree under a proxy-mode entry that has not been synced yet, or a
// directory nothing serves, exits 1 with the reason.
func runURL(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("url", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the config file (default: ~/Library/Application Support/herd-wake/config.yaml)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 1 {
		fmt.Fprintf(stderr, "herd-wake: usage: herd-wake url [--config path] [directory]\n")
		return 2
	}
	dir := flags.Arg(0)
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "herd-wake: %v\n", err)
			return 1
		}
		dir = cwd
	}
	dir, err := canonicalDir(dir)
	if err != nil {
		fmt.Fprintf(stderr, "herd-wake: %v\n", err)
		return 1
	}

	cfg, _, ok := loadConfigWithOptions(*configPath, config.LoadOptions{SkipWorkingDirectoryCheck: true}, stderr)
	if !ok {
		return 1
	}
	url, err := resolveURL(cfg, dir)
	if err != nil {
		fmt.Fprintf(stderr, "herd-wake: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, url)
	return 0
}

// canonicalDir makes dir absolute with symlinks resolved, and checks it is
// a directory.
func canonicalDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%s: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return resolved, nil
}

// resolveURL finds the public URL for the canonical directory dir: a
// registered project's working_directory (main config or projects.d,
// including proxy-mode managed projects), else a servable worktree under
// a wildcard entry's directory.
func resolveURL(cfg *config.Config, dir string) (string, error) {
	for _, name := range cfg.ProjectNames() {
		p := cfg.Projects[name]
		if sameDir(p.WorkingDirectory, dir) {
			return p.PublicURL, nil
		}
	}

	var reasons []string
	label := filepath.Base(dir)
	for _, d := range cfg.Discovery {
		if !sameDir(d.Directory, filepath.Dir(dir)) {
			continue
		}
		_, reason := discovery.Check(d, label)
		switch {
		case reason != "":
			reasons = append(reasons, fmt.Sprintf("discovery %q: %s", d.Name, reason))
		case d.Wildcard():
			return d.PublicURL(label), nil
		default:
			reasons = append(reasons, fmt.Sprintf("discovery %q: %s is a worktree it would register, but it is not in %s yet; run `herd-wake sync`", d.Name, dir, d.Source()))
		}
	}
	if len(reasons) == 0 {
		return "", fmt.Errorf("no URL for %s: it is not a registered project's working_directory and not a subdirectory of any discovery entry's directory", dir)
	}
	return "", errors.New("no URL for " + dir + ":\n  - " + strings.Join(reasons, "\n  - "))
}

// sameDir reports whether two paths name the same directory once made
// absolute with symlinks resolved.
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ca, err := canonicalDir(a)
	if err != nil {
		return false
	}
	cb, err := canonicalDir(b)
	if err != nil {
		return false
	}
	return ca == cb
}
