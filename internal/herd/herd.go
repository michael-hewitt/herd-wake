// Package herd wraps the Laravel Herd command-line tool for the pieces
// herd-wake needs: finding the binary, listing parked paths and linked
// sites (to refuse names that would shadow a PHP site), inspecting a site's
// nginx file, and creating or removing proxy entries.
//
// Everything that touches the machine is injectable (binary path, Herd
// config directory, PATH lookup) so tests run against a fake `herd`
// script, and the package compiles and tests on every platform even though
// Herd itself only exists on macOS.
package herd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned by Detect when no Herd CLI could be located.
var ErrNotFound = errors.New("herd CLI not found")

// Options configures Detect. Zero values mean "the real machine".
type Options struct {
	// Binary is an explicit path to the herd executable; when set, no
	// lookup happens.
	Binary string
	// ConfigDir is Herd's valet configuration directory, which holds the
	// Nginx/ site files. Default: ~/Library/Application Support/Herd/config/valet.
	ConfigDir string
	// Home overrides the home directory used for the default locations.
	Home string
	// LookPath resolves a command name on PATH. Default: exec.LookPath.
	LookPath func(file string) (string, error)
	// CommandTimeout bounds each CLI invocation. Default: 2 minutes (a
	// `herd proxy` restarts nginx and may issue a certificate).
	CommandTimeout time.Duration
}

// CLI is a resolved Herd command-line tool. Listing calls are cached for
// the CLI's lifetime, so one sync makes at most one `herd paths` and one
// `herd links` call however many names it checks.
type CLI struct {
	binary    string
	configDir string
	timeout   time.Duration

	mu    sync.Mutex
	paths *listing
	links *listing
}

// listing is a cached CLI listing.
type listing struct {
	items []string
	err   error
}

// Detect locates the Herd CLI: opts.Binary if set, else `herd` on PATH,
// else the bundled binary at ~/Library/Application Support/Herd/bin/herd.
// It returns ErrNotFound (wrapped) when none exists.
func Detect(opts Options) (*CLI, error) {
	home := opts.Home
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	configDir := opts.ConfigDir
	if configDir == "" {
		configDir = filepath.Join(home, "Library", "Application Support", "Herd", "config", "valet")
	}
	timeout := opts.CommandTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}

	binary := opts.Binary
	if binary == "" {
		lookPath := opts.LookPath
		if lookPath == nil {
			lookPath = exec.LookPath
		}
		if found, err := lookPath("herd"); err == nil {
			binary = found
		} else {
			bundled := filepath.Join(home, "Library", "Application Support", "Herd", "bin", "herd")
			if info, err := os.Stat(bundled); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				binary = bundled
			}
		}
	}
	if binary == "" {
		return nil, fmt.Errorf("%w: not on PATH and no bundled binary under %s", ErrNotFound,
			filepath.Join(home, "Library", "Application Support", "Herd", "bin"))
	}
	return &CLI{binary: binary, configDir: configDir, timeout: timeout}, nil
}

// Binary returns the resolved executable path.
func (c *CLI) Binary() string { return c.binary }

// SiteFile returns the path of the nginx site file Herd keeps for host
// (e.g. "dashboard.test"): <config dir>/Nginx/<host>. Herd writes one for
// every proxied or secured site; its absence means Herd has no explicit
// site for that host.
func (c *CLI) SiteFile(host string) string {
	return filepath.Join(c.configDir, "Nginx", host)
}

// SiteName returns the name Herd's CLI uses for host: the host without its
// TLD ("vite.accounts.test" → "vite.accounts"). ok is false when host has
// no TLD.
func SiteName(host string) (name string, ok bool) {
	i := strings.LastIndex(host, ".")
	if i <= 0 || i == len(host)-1 {
		return "", false
	}
	return host[:i], true
}

// SiteFromURL derives the Herd host and site name from a project's
// public_url ("https://vite.accounts.test" → "vite.accounts.test",
// "vite.accounts"). ok is false when the URL has no dotted host (a bare
// port or localhost URL is not a Herd site).
func SiteFromURL(publicURL string) (host, name string, ok bool) {
	u, err := url.Parse(publicURL)
	if err != nil {
		return "", "", false
	}
	host = strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil {
		return host, "", false
	}
	name, ok = SiteName(host)
	return host, name, ok
}

// ProxyCommand renders the shell command that registers a Herd proxy for
// name at the given supervisor port: what sync runs, and what it tells the
// user to run when it cannot.
func ProxyCommand(name string, port int) string {
	return fmt.Sprintf("herd proxy %s http://127.0.0.1:%d --secure", name, port)
}

// UnproxyCommand renders the shell command that removes a Herd proxy.
func UnproxyCommand(name string) string {
	return "herd unproxy " + name
}

// Proxy runs `herd proxy <name> http://127.0.0.1:<port> --secure`. Herd
// writes the site file, issues a certificate, and restarts nginx.
func (c *CLI) Proxy(ctx context.Context, name string, port int) error {
	_, err := c.run(ctx, "proxy", name, fmt.Sprintf("http://127.0.0.1:%d", port), "--secure")
	return err
}

// Unproxy runs `herd unproxy <name>`.
func (c *CLI) Unproxy(ctx context.Context, name string) error {
	_, err := c.run(ctx, "unproxy", name)
	return err
}

// proxyPass matches nginx proxy_pass directives in a site file.
var proxyPass = regexp.MustCompile(`proxy_pass\s+"?([^\s;"]+)"?\s*;`)

// ProxyTarget inspects host's site file and reports the port its
// proxy_pass directive targets. exists is false when there is no site file
// at all; port is 0 when the file exists but does not proxy (a plain PHP
// site secured with `herd secure`, say) or proxies somewhere unparsable.
func (c *CLI) ProxyTarget(host string) (port int, exists bool, err error) {
	data, err := os.ReadFile(c.SiteFile(host))
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read Herd site file: %w", err)
	}
	for _, m := range proxyPass.FindAllSubmatch(data, -1) {
		target := string(m[1])
		u, err := url.Parse(target)
		if err != nil || u.Port() == "" {
			continue
		}
		if p, err := strconv.Atoi(u.Port()); err == nil {
			return p, true, nil
		}
	}
	return 0, true, nil
}

// Paths returns Herd's parked paths (`herd paths`), cached after the first
// call.
func (c *CLI) Paths(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paths == nil {
		out, err := c.run(ctx, "paths")
		var items []string
		if err == nil {
			items = parsePaths(out)
		}
		c.paths = &listing{items: items, err: err}
	}
	return c.paths.items, c.paths.err
}

// Links returns the site names of Herd's linked sites (`herd links`),
// cached after the first call.
func (c *CLI) Links(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.links == nil {
		out, err := c.run(ctx, "links")
		var items []string
		if err == nil {
			items = parseLinks(out)
		}
		c.links = &listing{items: items, err: err}
	}
	return c.links.items, c.links.err
}

// Conflict reports whether registering a proxy for name would shadow an
// existing Herd site: a directory called name directly inside a parked
// path, or a linked site of that name. The returned reason is empty when
// the name is free. Listing failures are returned as err so callers can
// decide whether to proceed without the check.
func (c *CLI) Conflict(ctx context.Context, name string) (reason string, err error) {
	paths, err := c.Paths(ctx)
	if err != nil {
		return "", fmt.Errorf("list parked paths: %w", err)
	}
	for _, parked := range paths {
		candidate := filepath.Join(parked, name)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return fmt.Sprintf("a directory named %q exists in Herd parked path %s (%s); its site %s.test would be shadowed", name, parked, candidate, name), nil
		}
	}
	links, err := c.Links(ctx)
	if err != nil {
		return "", fmt.Errorf("list linked sites: %w", err)
	}
	for _, link := range links {
		if link == name {
			return fmt.Sprintf("%q is a linked Herd site (herd links); its site %s.test would be shadowed", name, name), nil
		}
	}
	return "", nil
}

// run executes one herd command and returns its stdout.
func (c *CLI) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail != "" {
			return nil, fmt.Errorf("herd %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return nil, fmt.Errorf("herd %s: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

// parsePaths reads `herd paths` output: a JSON array of directories, or —
// should Herd ever print plain text — one path per line.
func parsePaths(out []byte) []string {
	trimmed := bytes.TrimSpace(out)
	var paths []string
	if json.Unmarshal(trimmed, &paths) == nil {
		return cleanPaths(paths)
	}
	// Tolerate leading noise (warnings) before the JSON array.
	if i := bytes.IndexByte(trimmed, '['); i >= 0 {
		if json.Unmarshal(trimmed[i:], &paths) == nil {
			return cleanPaths(paths)
		}
	}
	for line := range strings.SplitSeq(string(trimmed), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/") || strings.HasPrefix(line, "~") {
			paths = append(paths, line)
		}
	}
	return cleanPaths(paths)
}

// cleanPaths trims trailing separators and drops duplicates, keeping order.
func cleanPaths(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > 1 {
			p = strings.TrimRight(p, "/")
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// parseLinks extracts site names from `herd links` output. The command
// prints a table (Site | Secured | URL | Path); a JSON array of objects or
// strings is accepted too. A site is recognised by its Site column, or by
// a URL column whose host is <site>.<tld>.
func parseLinks(out []byte) []string {
	trimmed := bytes.TrimSpace(out)
	names := map[string]bool{}

	var asJSON []any
	if len(trimmed) > 0 && trimmed[0] == '[' && json.Unmarshal(trimmed, &asJSON) == nil {
		for _, item := range asJSON {
			switch v := item.(type) {
			case string:
				names[v] = true
			case map[string]any:
				for _, key := range []string{"site", "Site", "name", "Name"} {
					if s, ok := v[key].(string); ok && s != "" {
						names[s] = true
					}
				}
				for _, key := range []string{"url", "URL", "Url"} {
					if s, ok := v[key].(string); ok {
						if _, name, ok := SiteFromURL(s); ok {
							names[name] = true
						}
					}
				}
			}
		}
		return sortedKeys(names)
	}

	for line := range strings.SplitSeq(string(trimmed), "\n") {
		if !strings.Contains(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		var trimmedCells []string
		for _, cell := range cells {
			if cell = strings.TrimSpace(cell); cell != "" {
				trimmedCells = append(trimmedCells, cell)
			}
		}
		if len(trimmedCells) == 0 || strings.EqualFold(trimmedCells[0], "site") {
			continue // header row
		}
		names[trimmedCells[0]] = true
		for _, cell := range trimmedCells[1:] {
			if strings.HasPrefix(cell, "http://") || strings.HasPrefix(cell, "https://") {
				if _, name, ok := SiteFromURL(cell); ok {
					names[name] = true
				}
			}
		}
	}
	return sortedKeys(names)
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
