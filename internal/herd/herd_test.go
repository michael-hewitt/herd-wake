package herd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// fake writes a fake herd script and returns a CLI bound to it plus the
// log file and site-file directory.
func fake(t *testing.T, f testproc.FakeHerd) (cli *CLI, logFile, nginxDir string) {
	t.Helper()
	dir := t.TempDir()
	configDir := filepath.Join(dir, "valet")
	f.LogFile = filepath.Join(dir, "herd.log")
	f.NginxDir = filepath.Join(configDir, "Nginx")
	bin, err := testproc.WriteFakeHerd(dir, f)
	if err != nil {
		t.Fatal(err)
	}
	cli, err = Detect(Options{Binary: bin, ConfigDir: configDir})
	if err != nil {
		t.Fatal(err)
	}
	return cli, f.LogFile, f.NginxDir
}

func TestDetectPrefersExplicitThenPathThenBundled(t *testing.T) {
	home := t.TempDir()
	bundled := filepath.Join(home, "Library", "Application Support", "Herd", "bin", "herd")
	notFound := func(string) (string, error) { return "", errors.New("not found") }

	// Nothing anywhere.
	if _, err := Detect(Options{Home: home, LookPath: notFound}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Detect with nothing installed: err = %v, want ErrNotFound", err)
	}

	// Bundled binary only.
	if err := os.MkdirAll(filepath.Dir(bundled), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundled, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // executable fixture
		t.Fatal(err)
	}
	cli, err := Detect(Options{Home: home, LookPath: notFound})
	if err != nil || cli.Binary() != bundled {
		t.Errorf("Detect bundled = %v, %v; want %s", cli, err, bundled)
	}
	if want := filepath.Join(home, "Library", "Application Support", "Herd", "config", "valet", "Nginx", "a.test"); cli.SiteFile("a.test") != want {
		t.Errorf("SiteFile = %q, want %q", cli.SiteFile("a.test"), want)
	}

	// PATH wins over bundled.
	onPath := func(string) (string, error) { return "/usr/local/bin/herd", nil }
	cli, err = Detect(Options{Home: home, LookPath: onPath})
	if err != nil || cli.Binary() != "/usr/local/bin/herd" {
		t.Errorf("Detect PATH = %v, %v", cli, err)
	}

	// Explicit wins over everything and skips the lookup.
	cli, err = Detect(Options{Binary: "/explicit/herd", Home: home, LookPath: func(string) (string, error) {
		t.Error("LookPath should not be called with an explicit binary")
		return "", nil
	}})
	if err != nil || cli.Binary() != "/explicit/herd" {
		t.Errorf("Detect explicit = %v, %v", cli, err)
	}
}

func TestSiteNames(t *testing.T) {
	for host, want := range map[string]string{
		"dashboard.test":     "dashboard",
		"vite.accounts.test": "vite.accounts",
		"localhost":          "",
		"":                   "",
		"trailing.":          "",
	} {
		got, ok := SiteName(host)
		if got != want || ok != (want != "") {
			t.Errorf("SiteName(%q) = %q, %v; want %q", host, got, ok, want)
		}
	}
	host, name, ok := SiteFromURL("https://Vite.Accounts.test/path?x=1")
	if !ok || host != "vite.accounts.test" || name != "vite.accounts" {
		t.Errorf("SiteFromURL = %q, %q, %v", host, name, ok)
	}
	if _, _, ok := SiteFromURL("http://127.0.0.1:7101"); ok {
		t.Error("an IP URL is not a Herd site")
	}
	if _, _, ok := SiteFromURL("http://localhost:3000"); ok {
		t.Error("localhost is not a Herd site")
	}
}

func TestCommands(t *testing.T) {
	if got := ProxyCommand("issue-1", 41000); got != "herd proxy issue-1 http://127.0.0.1:41000 --secure" {
		t.Errorf("ProxyCommand = %q", got)
	}
	if got := UnproxyCommand("issue-1"); got != "herd unproxy issue-1" {
		t.Errorf("UnproxyCommand = %q", got)
	}
}

func TestParsePaths(t *testing.T) {
	got := parsePaths([]byte("[\n  \"/Users/me/Herd\",\n  \"/Users/me/Herd/\",\n  \"/Users/me/Sites\"\n]\n"))
	if want := "/Users/me/Herd,/Users/me/Sites"; strings.Join(got, ",") != want {
		t.Errorf("parsePaths(JSON) = %v, want %s", got, want)
	}
	got = parsePaths([]byte("Some warning first\n[\"/a\"]"))
	if len(got) != 1 || got[0] != "/a" {
		t.Errorf("parsePaths(noise + JSON) = %v", got)
	}
	got = parsePaths([]byte("/x\n/y/\n"))
	if want := "/x,/y"; strings.Join(got, ",") != want {
		t.Errorf("parsePaths(lines) = %v, want %s", got, want)
	}
	if got := parsePaths([]byte("[]")); len(got) != 0 {
		t.Errorf("parsePaths([]) = %v", got)
	}
}

func TestParseLinks(t *testing.T) {
	table := `+-----------+---------+------------------------+------------------+
| Site      | Secured | URL                    | Path             |
+-----------+---------+------------------------+------------------+
| accounts  | X       | https://accounts.test  | /Users/me/acc    |
| shop      |         | http://shop.test       | /Users/me/shop   |
+-----------+---------+------------------------+------------------+
`
	if got := strings.Join(parseLinks([]byte(table)), ","); got != "accounts,shop" {
		t.Errorf("parseLinks(table) = %q", got)
	}
	if got := parseLinks([]byte("")); len(got) != 0 {
		t.Errorf("parseLinks(empty) = %v", got)
	}
	jsonOut := `[{"site":"api","secured":true,"url":"https://api.test","path":"/x"},"plain"]`
	if got := strings.Join(parseLinks([]byte(jsonOut)), ","); got != "api,plain" {
		t.Errorf("parseLinks(JSON) = %q", got)
	}
}

func TestListingsAreCachedAndConflictsDetected(t *testing.T) {
	parked := t.TempDir()
	if err := os.MkdirAll(filepath.Join(parked, "accounts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parked, "notadir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cli, logFile, _ := fake(t, testproc.FakeHerd{Parked: []string{parked}, Links: []string{"shop"}})
	ctx := context.Background()

	for i := range 3 {
		reason, err := cli.Conflict(ctx, "accounts")
		if err != nil || !strings.Contains(reason, "accounts") || !strings.Contains(reason, parked) {
			t.Errorf("round %d: Conflict(accounts) = %q, %v; want the parked directory named", i, reason, err)
		}
	}
	if reason, err := cli.Conflict(ctx, "shop"); err != nil || !strings.Contains(reason, "linked") {
		t.Errorf("Conflict(shop) = %q, %v; want a linked-site conflict", reason, err)
	}
	for _, free := range []string{"issue-1", "notadir"} {
		if reason, err := cli.Conflict(ctx, free); err != nil || reason != "" {
			t.Errorf("Conflict(%s) = %q, %v; want no conflict", free, reason, err)
		}
	}

	calls := testproc.ReadFakeHerdLog(logFile)
	if len(calls) != 2 || calls[0] != "paths" || calls[1] != "links" {
		t.Errorf("herd calls = %v, want exactly one paths and one links call", calls)
	}
}

func TestConflictReportsListingFailure(t *testing.T) {
	cli, _, _ := fake(t, testproc.FakeHerd{Fail: []string{"paths"}})
	_, err := cli.Conflict(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "paths failed") {
		t.Errorf("Conflict with a failing herd paths: err = %v", err)
	}
}

func TestProxyUnproxyAndProxyTarget(t *testing.T) {
	cli, logFile, nginxDir := fake(t, testproc.FakeHerd{})
	ctx := context.Background()

	if port, exists, err := cli.ProxyTarget("issue-1.test"); err != nil || exists || port != 0 {
		t.Errorf("ProxyTarget before proxy = %d, %v, %v", port, exists, err)
	}
	if err := cli.Proxy(ctx, "issue-1", 41000); err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nginxDir, "issue-1.test")); err != nil {
		t.Fatalf("site file not written: %v", err)
	}
	if port, exists, err := cli.ProxyTarget("issue-1.test"); err != nil || !exists || port != 41000 {
		t.Errorf("ProxyTarget after proxy = %d, %v, %v; want 41000", port, exists, err)
	}

	// A site file without proxy_pass (a secured PHP site) reports port 0.
	if err := os.WriteFile(filepath.Join(nginxDir, "php.test"), []byte("server {\n  listen 443 ssl;\n  root /x;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if port, exists, err := cli.ProxyTarget("php.test"); err != nil || !exists || port != 0 {
		t.Errorf("ProxyTarget(php) = %d, %v, %v; want exists with port 0", port, exists, err)
	}
	// Quoted targets parse too.
	if err := os.WriteFile(filepath.Join(nginxDir, "quoted.test"), []byte("location / {\n proxy_pass \"http://127.0.0.1:5173\";\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if port, _, _ := cli.ProxyTarget("quoted.test"); port != 5173 {
		t.Errorf("ProxyTarget(quoted) = %d, want 5173", port)
	}

	if err := cli.Unproxy(ctx, "issue-1"); err != nil {
		t.Fatalf("Unproxy: %v", err)
	}
	if _, exists, _ := cli.ProxyTarget("issue-1.test"); exists {
		t.Error("site file should be gone after unproxy")
	}

	calls := testproc.ReadFakeHerdLog(logFile)
	want := []string{"proxy issue-1 http://127.0.0.1:41000 --secure", "unproxy issue-1"}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("herd calls = %v, want %v", calls, want)
	}
}

func TestCommandFailureIncludesOutput(t *testing.T) {
	cli, _, _ := fake(t, testproc.FakeHerd{Fail: []string{"proxy"}})
	err := cli.Proxy(context.Background(), "x", 1)
	if err == nil || !strings.Contains(err.Error(), "herd proxy x") || !strings.Contains(err.Error(), "proxy failed") {
		t.Errorf("Proxy failure error = %v, want the command and its stderr", err)
	}
}
