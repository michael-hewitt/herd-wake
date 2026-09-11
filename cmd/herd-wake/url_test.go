package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// wildcardConfig rewrites the sync fixture's config with a wildcard entry
// (and the given projects body) instead of the proxy-mode one.
func (f *syncFixture) writeWildcardConfig(projects string) {
	f.t.Helper()
	command, err := testproc.Command()
	if err != nil {
		f.t.Fatal(err)
	}
	body := fmt.Sprintf(`projects:
%sdiscovery:
  - name: webapp
    mode: wildcard
    supervisor_port: %d
    directory: %s
    repository: %s
    require_files: [port]
    port_command: cat port
    command: HW_TESTPROC_PORT=$(cat port) %s
    readiness_strategy: tcp
    startup_timeout_seconds: 20
    shutdown_timeout_seconds: 5
    env:
      %s: %s
`, projects, f.supervisorLow, f.workspace, f.repo, command, testproc.EnvMode, testproc.ModeHTTP)
	if err := os.WriteFile(f.configPath, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// TestRunURL: `url` prints the wildcard URL for a servable worktree, a
// registered project's public_url for its working_directory (hand-written
// or synced), and exits 1 with the reason otherwise.
func TestRunURL(t *testing.T) {
	f := newSyncFixture(t)
	alpha, _ := f.worktree("alpha")
	beta, _ := f.worktree("beta")
	if err := os.Remove(filepath.Join(beta, "port")); err != nil {
		t.Fatal(err)
	}
	static := t.TempDir()
	f.writeWildcardConfig(fmt.Sprintf(`  dashboard:
    public_url: https://dashboard.test
    supervisor_port: 7101
    application_port: 17101
    working_directory: %s
    command: npm run dev
`, static))

	code, stdout, stderr := f.run("url", "--config", f.configPath, alpha)
	if code != 0 || strings.TrimSpace(stdout) != "https://alpha.webapp.test" {
		t.Errorf("url alpha = %d %q (stderr %q), want https://alpha.webapp.test", code, stdout, stderr)
	}
	// A relative path and a symlink resolve to the same worktree.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(alpha, link); err != nil {
		t.Fatal(err)
	}
	if code, stdout, _ := f.run("url", "--config", f.configPath, link); code != 0 || strings.TrimSpace(stdout) != "https://alpha.webapp.test" {
		t.Errorf("url via symlink = %d %q", code, stdout)
	}
	// The static project by working_directory.
	if code, stdout, _ := f.run("url", "--config", f.configPath, static); code != 0 || strings.TrimSpace(stdout) != "https://dashboard.test" {
		t.Errorf("url static = %d %q", code, stdout)
	}
	// Not a candidate: exit 1 with the rule that failed.
	code, stdout, stderr = f.run("url", "--config", f.configPath, beta)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "missing required file(s): port") {
		t.Errorf("url beta = %d %q %q, want exit 1 naming the missing file", code, stdout, stderr)
	}
	// Under no entry at all.
	code, _, stderr = f.run("url", "--config", f.configPath, t.TempDir())
	if code != 1 || !strings.Contains(stderr, "not a subdirectory of any discovery entry") {
		t.Errorf("url unrelated = %d %q", code, stderr)
	}
	// A missing directory.
	if code, _, stderr := f.run("url", "--config", f.configPath, filepath.Join(f.workspace, "nope")); code != 1 || !strings.Contains(stderr, "nope") {
		t.Errorf("url missing = %d %q", code, stderr)
	}
	// Default: the current directory.
	wd, _ := os.Getwd()
	t.Chdir(alpha)
	code, stdout, _ = f.run("url", "--config", f.configPath)
	t.Chdir(wd)
	if code != 0 || strings.TrimSpace(stdout) != "https://alpha.webapp.test" {
		t.Errorf("url (cwd) = %d %q", code, stdout)
	}
	if code, _, stderr := f.run("url", "--config", f.configPath, "a", "b"); code != 2 || !strings.Contains(stderr, "usage") {
		t.Errorf("url with two args = %d %q", code, stderr)
	}
}

// TestRunURLProxyModeNeedsSync: under a proxy-mode entry, a worktree has a
// URL only once sync registered it.
func TestRunURLProxyModeNeedsSync(t *testing.T) {
	f := newSyncFixture(t)
	alpha, _ := f.worktree("alpha")
	f.writeConfig("")

	code, _, stderr := f.run("url", "--config", f.configPath, alpha)
	if code != 1 || !strings.Contains(stderr, "run `herd-wake sync`") {
		t.Errorf("url before sync = %d %q", code, stderr)
	}
	if code, _, stderr := f.run("sync", "--config", f.configPath, "--socket", f.socket, "--no-herd"); code != 0 {
		t.Fatalf("sync: %d %q", code, stderr)
	}
	if code, stdout, stderr := f.run("url", "--config", f.configPath, alpha); code != 0 || strings.TrimSpace(stdout) != "https://alpha.test" {
		t.Errorf("url after sync = %d %q %q", code, stdout, stderr)
	}
}

// TestRunSyncWildcardEntry: sync on a wildcard entry proxies the base name
// once, writes no projects.d file, reports the URL pattern, and reloads
// the daemon, which then serves worktrees by Host; a second sync calls
// herd not at all.
func TestRunSyncWildcardEntry(t *testing.T) {
	f := newSyncFixture(t)
	f.worktree("alpha")
	f.writeWildcardConfig("")
	f.startDaemon()

	code, stdout, stderr := f.run("sync", "--config", f.configPath, "--socket", f.socket)
	if code != 0 {
		t.Fatalf("sync exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		fmt.Sprintf(`Discovery "webapp" (%s) → wildcard: https://<label>.webapp.test → %s/<label>`, f.workspace, f.workspace),
		fmt.Sprintf("herd:      proxy webapp → http://127.0.0.1:%d (done)", f.supervisorLow),
		"Daemon: reloaded — added (none), removed (none), changed (none), unchanged (none)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("sync output missing %q; got:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "file:") {
		t.Errorf("wildcard sync output should not mention a managed file:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(f.configDir, "projects.d")); !os.IsNotExist(err) {
		t.Error("sync created projects.d for a wildcard entry")
	}
	calls := testproc.ReadFakeHerdLog(f.herdLog)
	if proxies := countPrefix(calls, "proxy "); proxies != 1 || calls[len(calls)-1] != fmt.Sprintf("proxy webapp http://127.0.0.1:%d --secure", f.supervisorLow) {
		t.Errorf("herd calls = %v, want exactly one proxy of the base name", calls)
	}

	// The daemon serves the worktree by Host through the shared port.
	code, body := getWithHost(t, f.supervisorLow, "alpha.webapp.test", "/hello")
	if code != 200 || !strings.Contains(body, "ok GET /hello host=alpha.webapp.test") {
		t.Fatalf("GET alpha = %d %q", code, body)
	}
	code, body = getWithHost(t, f.supervisorLow, "nope.webapp.test", "/")
	if code != 404 || !strings.Contains(body, `no worktree named "nope"`) {
		t.Errorf("GET nope = %d %q", code, body)
	}

	// status shows the entry and the dynamic project.
	code, stdout, _ = f.run("status", "--socket", f.socket)
	if code != 0 {
		t.Fatalf("status failed:\n%s", stdout)
	}
	for _, want := range []string{"wildcard webapp: https://<label>.webapp.test", "1 project(s) materialised", "alpha", "discovery:webapp (dynamic)", "SOURCE"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output missing %q; got:\n%s", want, stdout)
		}
	}

	// project:* commands work by label.
	if code, stdout, stderr := f.run("project:stop", "--socket", f.socket, "alpha"); code != 0 || !strings.Contains(stdout, `Project "alpha" is stopped`) {
		t.Errorf("project:stop alpha = %d %q %q", code, stdout, stderr)
	}

	// A second sync runs no herd command and still reloads.
	before := len(testproc.ReadFakeHerdLog(f.herdLog))
	code, stdout, stderr = f.run("sync", "--config", f.configPath, "--socket", f.socket)
	if code != 0 {
		t.Fatalf("second sync exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "skipped; already proxied") {
		t.Errorf("second sync output missing the skip; got:\n%s", stdout)
	}
	if after := len(testproc.ReadFakeHerdLog(f.herdLog)); after != before {
		t.Errorf("second sync ran herd %d time(s)", after-before)
	}

	// `url` agrees with the daemon.
	if code, stdout, _ := f.run("url", "--config", f.configPath, filepath.Join(f.workspace, "alpha")); code != 0 || strings.TrimSpace(stdout) != "https://alpha.webapp.test" {
		t.Errorf("url = %d %q", code, stdout)
	}
}

func countPrefix(list []string, prefix string) int {
	n := 0
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// getWithHost performs GET http://127.0.0.1:port/path with the given Host.
func getWithHost(t *testing.T, port int, host, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", host, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestUsageMentionsURL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run(nil, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "url [directory]") {
		t.Errorf("usage should mention the url command; got:\n%s", stderr.String())
	}
}
