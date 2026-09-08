package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/herd"
	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// fixture is a config directory, a workspace of fake worktrees, the
// repository they belong to, and a fake herd.
type fixture struct {
	t          *testing.T
	configDir  string
	configPath string
	workspace  string
	repo       string
	herdLog    string
	nginxDir   string
	herdBin    string
	parked     string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t:         t,
		configDir: t.TempDir(),
		workspace: t.TempDir(),
		repo:      t.TempDir(),
		parked:    t.TempDir(),
	}
	f.configPath = filepath.Join(f.configDir, "config.yaml")
	if err := os.MkdirAll(filepath.Join(f.repo, ".git", "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.herdLog = filepath.Join(t.TempDir(), "herd.log")
	f.nginxDir = filepath.Join(t.TempDir(), "valet", "Nginx")
	return f
}

// worktree creates a linked worktree of the fixture repository (a .git
// file pointing into <repo>/.git/worktrees/<name>) with the given files.
func (f *fixture) worktree(name string, files ...string) string {
	f.t.Helper()
	dir := filepath.Join(f.workspace, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	admin := filepath.Join(f.repo, ".git", "worktrees", name)
	if err := os.MkdirAll(admin, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("x"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	return dir
}

// standaloneRepo creates a directory with its own .git directory.
func (f *fixture) standaloneRepo(name string) string {
	f.t.Helper()
	dir := filepath.Join(f.workspace, name, ".git")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	return filepath.Dir(dir)
}

// writeConfig writes the main config: projects body (may be empty) plus a
// discovery entry with extra template lines.
func (f *fixture) writeConfig(projects string, discoveryExtra string) {
	f.t.Helper()
	body := "projects:\n" + projects + fmt.Sprintf(`discovery:
  - name: webapp
    directory: %s
    repository: %s
    supervisor_port_range: [41000, 41999]
    application_port_range: [42000, 42999]
    command: ./start.sh
%s`, f.workspace, f.repo, discoveryExtra)
	if err := os.WriteFile(f.configPath, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) load() *config.Config {
	f.t.Helper()
	cfg, err := config.LoadWithOptions(f.configPath, config.LoadOptions{SkipWorkingDirectoryCheck: true})
	if err != nil {
		f.t.Fatalf("load config: %v", err)
	}
	return cfg
}

// herd builds the fake herd (parked path = f.parked) and a CLI for it.
func (f *fixture) herd(extra testproc.FakeHerd) *herd.CLI {
	f.t.Helper()
	extra.LogFile = f.herdLog
	extra.NginxDir = f.nginxDir
	extra.Parked = append(extra.Parked, f.parked)
	bin, err := testproc.WriteFakeHerd(f.t.TempDir(), extra)
	if err != nil {
		f.t.Fatal(err)
	}
	f.herdBin = bin
	cli, err := herd.Detect(herd.Options{Binary: bin, ConfigDir: filepath.Dir(f.nginxDir)})
	if err != nil {
		f.t.Fatal(err)
	}
	return cli
}

func (f *fixture) sync(opts Options) *EntryResult {
	f.t.Helper()
	res, err := Sync(context.Background(), f.load(), opts)
	if err != nil {
		f.t.Fatalf("Sync: %v", err)
	}
	if len(res.Entries) != 1 {
		f.t.Fatalf("Sync returned %d entries, want 1", len(res.Entries))
	}
	return res.Entries[0]
}

func (f *fixture) managedFile() string {
	return filepath.Join(f.configDir, "projects.d", "webapp.yaml")
}

func (f *fixture) managed() map[string]*config.Project {
	f.t.Helper()
	projects, err := config.DecodeFragment(f.managedFile())
	if err != nil {
		f.t.Fatalf("decode managed file: %v", err)
	}
	return projects
}

func (f *fixture) herdCalls() []string {
	return testproc.ReadFakeHerdLog(f.herdLog)
}

// mutations returns the herd calls that change something.
func (f *fixture) herdMutations() []string {
	var out []string
	for _, call := range f.herdCalls() {
		if strings.HasPrefix(call, "proxy ") || strings.HasPrefix(call, "unproxy ") {
			out = append(out, call)
		}
	}
	return out
}

func names(list []ProjectSummary) string {
	var out []string
	for _, p := range list {
		out = append(out, p.Name)
	}
	return strings.Join(out, ",")
}

func skipNames(list []Skip) string {
	var out []string
	for _, s := range list {
		out = append(out, s.Name)
	}
	return strings.Join(out, ",")
}

func findSkip(t *testing.T, list []Skip, name string) Skip {
	t.Helper()
	for _, s := range list {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no skip for %q in %v", name, list)
	return Skip{}
}

func findHerd(t *testing.T, list []HerdAction, action, project string) HerdAction {
	t.Helper()
	for _, a := range list {
		if a.Action == action && a.Project == project {
			return a
		}
	}
	t.Fatalf("no %s action for %q in %+v", action, project, list)
	return HerdAction{}
}

func TestSyncWritesManagedFileWithStablePorts(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"issue-1", "issue-2", "issue-3"} {
		f.worktree(name)
	}
	f.writeConfig("", "    env: { ENVIRONMENT: dev }\n    idle_timeout_minutes: 30\n")
	mainBefore, _ := os.ReadFile(f.configPath)
	cli := f.herd(testproc.FakeHerd{})

	res := f.sync(Options{Herd: cli})
	if len(res.Errors) > 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
	if names(res.Added) != "issue-1,issue-2,issue-3" || !res.Written || !res.Changed {
		t.Fatalf("first sync: added=%q written=%v changed=%v", names(res.Added), res.Written, res.Changed)
	}
	projects := f.managed()
	if len(projects) != 3 {
		t.Fatalf("managed file has %d projects, want 3", len(projects))
	}
	p := projects["issue-2"]
	if p.PublicURL != "https://issue-2.test" || p.WorkingDirectory != filepath.Join(f.workspace, "issue-2") || p.Command != "./start.sh" {
		t.Errorf("issue-2 = %+v", p)
	}
	if p.SupervisorPort != 41001 || p.ApplicationPort != 42001 {
		t.Errorf("issue-2 ports = %d/%d, want 41001/42001 (lowest free, in name order)", p.SupervisorPort, p.ApplicationPort)
	}
	if p.Env["ENVIRONMENT"] != "dev" || p.IdleTimeoutMinutes != 30 {
		t.Errorf("template fields not copied: %+v", p)
	}
	if p.ReadinessStrategy != "" || p.ShutdownSignal != "" {
		t.Errorf("managed file should not pin defaults: %+v", p)
	}
	content, _ := os.ReadFile(f.managedFile())
	if !strings.HasPrefix(string(content), managedMarker) || !strings.Contains(string(content), "do not edit") {
		t.Errorf("managed file lacks the header:\n%s", content)
	}
	if mainAfter, _ := os.ReadFile(f.configPath); string(mainAfter) != string(mainBefore) {
		t.Error("main config.yaml was modified")
	}
	// The whole file loads as a valid configuration.
	if _, err := config.Load(f.configPath); err != nil {
		t.Errorf("generated config does not load: %v", err)
	}

	// Herd: one proxy per new project.
	want := []string{
		"proxy issue-1 http://127.0.0.1:41000 --secure",
		"proxy issue-2 http://127.0.0.1:41001 --secure",
		"proxy issue-3 http://127.0.0.1:41002 --secure",
	}
	if got := f.herdMutations(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("herd calls = %v, want %v", got, want)
	}

	// Second run: nothing changes, ports are stable, no herd mutations.
	res = f.sync(Options{Herd: f.herd(testproc.FakeHerd{})})
	if names(res.Unchanged) != "issue-1,issue-2,issue-3" || len(res.Added) != 0 || res.Changed || res.Written {
		t.Fatalf("second sync: unchanged=%q added=%d changed=%v written=%v", names(res.Unchanged), len(res.Added), res.Changed, res.Written)
	}
	if got := f.managed()["issue-2"]; got.SupervisorPort != 41001 || got.ApplicationPort != 42001 {
		t.Errorf("ports drifted on second sync: %d/%d", got.SupervisorPort, got.ApplicationPort)
	}
	if got := f.herdMutations(); len(got) != 3 {
		t.Errorf("second sync ran herd mutations: %v", got)
	}
	for _, a := range res.Herd {
		if a.Status != HerdSkipped || !strings.Contains(a.Detail, "already proxied") {
			t.Errorf("second sync herd action = %+v, want skipped/already proxied", a)
		}
	}
}

func TestSyncRemovesDeletedWorktreeAndUnproxies(t *testing.T) {
	f := newFixture(t)
	f.worktree("keep")
	gone := f.worktree("gone")
	f.writeConfig("", "")
	f.sync(Options{Herd: f.herd(testproc.FakeHerd{})})
	if _, err := os.Stat(filepath.Join(f.nginxDir, "gone.test")); err != nil {
		t.Fatalf("site file for gone not created: %v", err)
	}

	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	// The stale project's working_directory is gone; sync must still load.
	res := f.sync(Options{Herd: f.herd(testproc.FakeHerd{})})
	if names(res.Removed) != "gone" || names(res.Unchanged) != "keep" || len(res.Added) != 0 {
		t.Fatalf("removed=%q unchanged=%q added=%q", names(res.Removed), names(res.Unchanged), names(res.Added))
	}
	if _, ok := f.managed()["gone"]; ok {
		t.Error("gone should be removed from the managed file")
	}
	if _, ok := f.managed()["keep"]; !ok {
		t.Error("keep should still be in the managed file")
	}
	calls := f.herdMutations()
	if len(calls) != 3 || calls[2] != "unproxy gone" {
		t.Errorf("herd calls = %v, want the two proxies then exactly `unproxy gone`", calls)
	}
	if a := findHerd(t, res.Herd, "unproxy", "gone"); a.Status != HerdDone {
		t.Errorf("unproxy action = %+v", a)
	}
	if _, err := os.Stat(filepath.Join(f.nginxDir, "gone.test")); !os.IsNotExist(err) {
		t.Error("site file for gone should be removed")
	}
	// Its ports are free again for the next new worktree.
	f.worktree("next")
	f.sync(Options{Herd: f.herd(testproc.FakeHerd{})})
	if next := f.managed()["next"]; next == nil || next.SupervisorPort != 41000 || next.ApplicationPort != 42000 {
		t.Errorf("next should reuse the freed ports 41000/42000: %+v", next)
	}
}

func TestSyncProxiesOnlyWhenSiteFileMissingOrWrong(t *testing.T) {
	f := newFixture(t)
	f.worktree("pre")
	f.worktree("stale")
	f.worktree("fresh")
	f.writeConfig("", "")
	cli := f.herd(testproc.FakeHerd{})
	// pre is already proxied to the port sync will allocate (41002 by name
	// order: fresh=41000, pre=41001, stale=41002).
	site := func(name string, port int) {
		if err := os.WriteFile(filepath.Join(f.nginxDir, name+".test"),
			[]byte(fmt.Sprintf("location / {\n  proxy_pass http://127.0.0.1:%d;\n}\n", port)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	site("pre", 41001)
	site("stale", 7777)

	res := f.sync(Options{Herd: cli})
	if a := findHerd(t, res.Herd, "proxy", "pre"); a.Status != HerdSkipped {
		t.Errorf("pre should be skipped (already proxied): %+v", a)
	}
	if a := findHerd(t, res.Herd, "proxy", "stale"); a.Status != HerdDone || !strings.Contains(a.Detail, "re-pointed from 127.0.0.1:7777") {
		t.Errorf("stale should be re-pointed: %+v", a)
	}
	if a := findHerd(t, res.Herd, "proxy", "fresh"); a.Status != HerdDone {
		t.Errorf("fresh should be proxied: %+v", a)
	}
	want := []string{"proxy fresh http://127.0.0.1:41000 --secure", "proxy stale http://127.0.0.1:41002 --secure"}
	if got := f.herdMutations(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("herd calls = %v, want %v", got, want)
	}
}

func TestSyncRefusesNamesThatShadowHerdSites(t *testing.T) {
	f := newFixture(t)
	f.worktree("accounts") // a parked PHP site of that name exists
	f.worktree("shop")     // a linked site of that name exists
	f.worktree("secured")  // Herd has a non-proxy site file for it
	f.worktree("free")
	if err := os.MkdirAll(filepath.Join(f.parked, "accounts"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.writeConfig("", "")
	cli := f.herd(testproc.FakeHerd{Links: []string{"shop"}})
	if err := os.WriteFile(filepath.Join(f.nginxDir, "secured.test"), []byte("server {\n  root /x;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := f.sync(Options{Herd: cli})
	if names(res.Added) != "free" {
		t.Fatalf("added = %q, want only free", names(res.Added))
	}
	for name, want := range map[string]string{
		"accounts": "parked path",
		"shop":     "linked Herd site",
		"secured":  "not a proxy",
	} {
		s := findSkip(t, res.Skipped, name)
		if !strings.HasPrefix(s.Reason, "refused:") || !strings.Contains(s.Reason, want) {
			t.Errorf("%s skip reason = %q, want a refusal mentioning %q", name, s.Reason, want)
		}
	}
	if s := findSkip(t, res.Skipped, "accounts"); !strings.Contains(s.Reason, filepath.Join(f.parked, "accounts")) {
		t.Errorf("accounts refusal should name the conflicting directory: %q", s.Reason)
	}
	projects := f.managed()
	if len(projects) != 1 || projects["free"] == nil {
		t.Errorf("managed file should hold only free: %v", projects)
	}
	if got := f.herdMutations(); len(got) != 1 || got[0] != "proxy free http://127.0.0.1:41000 --secure" {
		t.Errorf("herd calls = %v, want only free proxied", got)
	}
	// One listing call each, however many names were checked.
	paths, links := 0, 0
	for _, call := range f.herdCalls() {
		switch call {
		case "paths":
			paths++
		case "links":
			links++
		}
	}
	if paths != 1 || links != 1 {
		t.Errorf("herd paths/links called %d/%d times, want once each", paths, links)
	}
}

func TestSyncPortCommand(t *testing.T) {
	f := newFixture(t)
	bare := f.worktree("bare")
	asURL := f.worktree("as-url")
	broken := f.worktree("broken")
	writePortScript := func(dir, body string) {
		if err := os.WriteFile(filepath.Join(dir, "port.sh"), []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // test script
			t.Fatal(err)
		}
	}
	writePortScript(bare, "echo \"env=$ENVIRONMENT\" >&2\necho 45001\n")
	writePortScript(asURL, "echo https://as-url.test:45002/\n")
	writePortScript(broken, "echo nope >&2\nexit 3\n")
	f.writeConfig("", "    port_command: sh port.sh\n    env: { ENVIRONMENT: dev }\n")

	res := f.sync(Options{})
	if names(res.Added) != "as-url,bare" {
		t.Fatalf("added = %q (skipped: %+v)", names(res.Added), res.Skipped)
	}
	projects := f.managed()
	if projects["bare"].ApplicationPort != 45001 {
		t.Errorf("bare application_port = %d, want 45001", projects["bare"].ApplicationPort)
	}
	if projects["as-url"].ApplicationPort != 45002 {
		t.Errorf("as-url application_port = %d, want 45002", projects["as-url"].ApplicationPort)
	}
	s := findSkip(t, res.Skipped, "broken")
	if !strings.Contains(s.Reason, "port_command failed") || !strings.Contains(s.Reason, "exit status 3") || !strings.Contains(s.Reason, "nope") {
		t.Errorf("broken skip reason = %q", s.Reason)
	}
	if _, ok := projects["broken"]; ok {
		t.Error("broken must not be written")
	}

	// The command is re-run every sync: a changed port updates the project.
	writePortScript(bare, "echo 45010\n")
	res = f.sync(Options{})
	if names(res.Updated) != "bare" || !strings.Contains(res.Updated[0].Detail, "45001 → 45010") {
		t.Errorf("updated = %+v", res.Updated)
	}
	if f.managed()["bare"].ApplicationPort != 45010 {
		t.Error("bare application_port not updated")
	}
	// The supervisor port is untouched by the update.
	if f.managed()["bare"].SupervisorPort != 41001 {
		t.Errorf("bare supervisor_port = %d, want the original 41001", f.managed()["bare"].SupervisorPort)
	}

	// A failing port_command on an existing project keeps it as it was.
	writePortScript(bare, "exit 1\n")
	res = f.sync(Options{})
	s = findSkip(t, res.Skipped, "bare")
	if !strings.Contains(s.Reason, "previous registration kept") {
		t.Errorf("bare skip reason = %q, want it kept", s.Reason)
	}
	if names(res.Unchanged) != "as-url,bare" {
		t.Errorf("unchanged = %q", names(res.Unchanged))
	}
	if f.managed()["bare"].ApplicationPort != 45010 {
		t.Error("bare should keep its previous registration")
	}

	// Two worktrees printing the same port: the second is skipped.
	writePortScript(bare, "echo 45002\n")
	res = f.sync(Options{})
	s = findSkip(t, res.Skipped, "bare")
	if !strings.Contains(s.Reason, "45002") || !strings.Contains(s.Reason, `"as-url"`) {
		t.Errorf("duplicate port skip reason = %q", s.Reason)
	}
}

func TestSyncPortCommandTimeout(t *testing.T) {
	f := newFixture(t)
	f.worktree("slow")
	f.writeConfig("", "    port_command: sleep 5; echo 45000\n")
	start := time.Now()
	res := f.sync(Options{PortCommandTimeout: 200 * time.Millisecond})
	if time.Since(start) > 3*time.Second {
		t.Errorf("port_command timeout not enforced (took %s)", time.Since(start))
	}
	s := findSkip(t, res.Skipped, "slow")
	if !strings.Contains(s.Reason, "timed out") {
		t.Errorf("slow skip reason = %q", s.Reason)
	}
}

func TestParsePort(t *testing.T) {
	for output, want := range map[string]int{
		"45001\n":                        45001,
		"  45001  ":                      45001,
		"https://x.test:5173/":           5173,
		"http://127.0.0.1:3000":          3000,
		"https://x.test":                 443,
		"http://x.test/":                 80,
		"warming up\n45002\n":            45002,
		"dev server on port 45003":       45003,
		"http://localhost:5173/ ready\n": 5173,
	} {
		got, err := parsePort(output)
		if want == 0 {
			if err == nil {
				t.Errorf("parsePort(%q) = %d, want error", output, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("parsePort(%q) = %d, %v; want %d", output, got, err, want)
		}
	}
	if _, err := parsePort(""); err == nil {
		t.Error("empty output should fail")
	}
	if _, err := parsePort("70000"); err == nil {
		t.Error("out-of-range port should fail")
	}
}

func TestScanRules(t *testing.T) {
	f := newFixture(t)
	f.worktree("good", "start.sh")
	f.worktree("Bad_Name", "start.sh")
	f.worktree("no-start")
	f.standaloneRepo("data-repo")
	other := t.TempDir() // a worktree of some other repository
	if err := os.MkdirAll(filepath.Join(other, ".git", "worktrees", "foreign"), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(f.workspace, "foreign")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, ".git"), []byte("gitdir: "+filepath.Join(other, ".git", "worktrees", "foreign")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"notes", ".hidden", "node_modules"} {
		if err := os.MkdirAll(filepath.Join(f.workspace, plain), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(f.workspace, ".hidden", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.workspace, "a-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.writeConfig("", "    require_files: [start.sh]\n")

	res := f.sync(Options{})
	if names(res.Added) != "good" {
		t.Errorf("added = %q, want only good", names(res.Added))
	}
	if got := skipNames(res.Skipped); got != "Bad_Name,data-repo,foreign,no-start" {
		t.Errorf("skipped = %q", got)
	}
	for name, want := range map[string]string{
		"Bad_Name":  "not a valid DNS label",
		"data-repo": "standalone git repository",
		"foreign":   "different repository",
		"no-start":  "missing required file(s): start.sh",
	} {
		if s := findSkip(t, res.Skipped, name); !strings.Contains(s.Reason, want) {
			t.Errorf("%s reason = %q, want %q", name, s.Reason, want)
		}
	}
}

func TestScanWithoutRepositoryAcceptsAnyGitDirectory(t *testing.T) {
	f := newFixture(t)
	f.worktree("linked")
	f.standaloneRepo("standalone")
	f.writeConfig("", "")
	// Drop the repository filter.
	data, _ := os.ReadFile(f.configPath)
	if err := os.WriteFile(f.configPath, []byte(strings.ReplaceAll(string(data), "    repository: "+f.repo+"\n", "")), 0o644); err != nil {
		t.Fatal(err)
	}
	res := f.sync(Options{})
	if names(res.Added) != "linked,standalone" {
		t.Errorf("added = %q, want both", names(res.Added))
	}
}

func TestSyncDryRunWritesNothingAndRunsNoHerdMutation(t *testing.T) {
	f := newFixture(t)
	f.worktree("issue-1")
	f.writeConfig("", "")
	cli := f.herd(testproc.FakeHerd{})

	res := f.sync(Options{Herd: cli, DryRun: true})
	if names(res.Added) != "issue-1" || !res.Changed || res.Written {
		t.Errorf("dry run: added=%q changed=%v written=%v", names(res.Added), res.Changed, res.Written)
	}
	if _, err := os.Stat(f.managedFile()); !os.IsNotExist(err) {
		t.Error("dry run wrote the managed file")
	}
	if _, err := os.Stat(filepath.Join(f.configDir, "projects.d")); !os.IsNotExist(err) {
		t.Error("dry run created projects.d")
	}
	if got := f.herdMutations(); len(got) != 0 {
		t.Errorf("dry run ran herd mutations: %v", got)
	}
	if a := findHerd(t, res.Herd, "proxy", "issue-1"); a.Status != HerdDryRun || a.Command != "herd proxy issue-1 http://127.0.0.1:41000 --secure" {
		t.Errorf("dry-run herd action = %+v", a)
	}
}

func TestSyncWithoutHerdReportsManualCommands(t *testing.T) {
	f := newFixture(t)
	f.worktree("issue-1")
	f.writeConfig("", "")

	res, err := Sync(context.Background(), f.load(), Options{HerdNote: "herd CLI not found"})
	if err != nil {
		t.Fatal(err)
	}
	e := res.Entries[0]
	if names(e.Added) != "issue-1" || !e.Written {
		t.Fatalf("without herd: added=%q written=%v", names(e.Added), e.Written)
	}
	a := findHerd(t, e.Herd, "proxy", "issue-1")
	if a.Status != HerdManual || a.Detail != "herd CLI not found" {
		t.Errorf("herd action = %+v, want manual", a)
	}
	if got := res.ManualCommands(); len(got) != 1 || got[0] != "herd proxy issue-1 http://127.0.0.1:41000 --secure" {
		t.Errorf("ManualCommands = %v", got)
	}
	if res.HerdAvailable {
		t.Error("HerdAvailable should be false")
	}
}

func TestSyncHerdDisabledPerEntry(t *testing.T) {
	f := newFixture(t)
	f.worktree("issue-1")
	f.writeConfig("", "    herd: false\n")
	cli := f.herd(testproc.FakeHerd{})
	res := f.sync(Options{Herd: cli})
	if a := findHerd(t, res.Herd, "proxy", "issue-1"); a.Status != HerdManual || !strings.Contains(a.Detail, "herd: false") {
		t.Errorf("herd action = %+v", a)
	}
	if got := f.herdMutations(); len(got) != 0 {
		t.Errorf("herd: false ran mutations: %v", got)
	}
}

func TestSyncHerdCommandFailureIsReportedNotFatal(t *testing.T) {
	f := newFixture(t)
	f.worktree("issue-1")
	f.writeConfig("", "")
	cli := f.herd(testproc.FakeHerd{Fail: []string{"proxy"}})
	res := f.sync(Options{Herd: cli})
	if len(res.Errors) > 0 || !res.Written {
		t.Fatalf("a failing herd proxy must not fail the sync: errors=%v written=%v", res.Errors, res.Written)
	}
	a := findHerd(t, res.Herd, "proxy", "issue-1")
	if a.Status != HerdManual || !strings.Contains(a.Detail, "proxy failed") {
		t.Errorf("herd action = %+v, want manual with the failure", a)
	}
}

func TestSyncRefusesUnmanagedFile(t *testing.T) {
	f := newFixture(t)
	f.worktree("issue-1")
	f.writeConfig("", "")
	if err := os.MkdirAll(filepath.Dir(f.managedFile()), 0o755); err != nil {
		t.Fatal(err)
	}
	hand := "projects:\n  hand:\n    public_url: https://hand.test\n    supervisor_port: 7101\n    application_port: 17101\n    working_directory: /tmp\n    command: npm run dev\n"
	if err := os.WriteFile(f.managedFile(), []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}
	res := f.sync(Options{})
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "not managed by herd-wake sync") {
		t.Fatalf("errors = %v, want a refusal", res.Errors)
	}
	if data, _ := os.ReadFile(f.managedFile()); string(data) != hand {
		t.Error("hand-written file was modified")
	}
}

func TestSyncSkipsNamesDefinedElsewhere(t *testing.T) {
	f := newFixture(t)
	f.worktree("dashboard")
	f.worktree("issue-1")
	f.writeConfig(`  dashboard:
    public_url: https://dashboard.test
    supervisor_port: 41000
    application_port: 42000
    working_directory: /tmp
    command: npm run dev
`, "")
	res := f.sync(Options{})
	if names(res.Added) != "issue-1" {
		t.Errorf("added = %q", names(res.Added))
	}
	if s := findSkip(t, res.Skipped, "dashboard"); !strings.Contains(s.Reason, "already defined in config.yaml") {
		t.Errorf("dashboard reason = %q", s.Reason)
	}
	// Ports used by the main config are never allocated.
	if p := f.managed()["issue-1"]; p.SupervisorPort != 41001 || p.ApplicationPort != 42001 {
		t.Errorf("issue-1 ports = %d/%d, want 41001/42001 (41000/42000 are taken by dashboard)", p.SupervisorPort, p.ApplicationPort)
	}
}

func TestSyncTemplateChangeUpdatesProjects(t *testing.T) {
	f := newFixture(t)
	f.worktree("issue-1")
	f.writeConfig("", "")
	f.sync(Options{})
	f.writeConfig("", "    rewrite_host: true\n")
	res := f.sync(Options{})
	if names(res.Updated) != "issue-1" || res.Updated[0].Detail != "template settings changed" {
		t.Errorf("updated = %+v", res.Updated)
	}
	if !f.managed()["issue-1"].RewriteHost {
		t.Error("rewrite_host not propagated")
	}
}

func TestSyncEmptyWorkspaceWritesEmptyManagedFile(t *testing.T) {
	f := newFixture(t)
	f.writeConfig("", "")
	res := f.sync(Options{})
	if len(res.Errors) > 0 || !res.Written {
		t.Fatalf("errors=%v written=%v", res.Errors, res.Written)
	}
	if projects := f.managed(); len(projects) != 0 {
		t.Errorf("managed file should be empty: %v", projects)
	}
	if _, err := config.Load(f.configPath); err != nil {
		t.Errorf("empty managed file should load: %v", err)
	}
}

func TestRemove(t *testing.T) {
	f := newFixture(t)
	f.worktree("issue-1")
	f.worktree("issue-2")
	f.writeConfig(`  dashboard:
    public_url: https://dashboard.test
    supervisor_port: 7101
    application_port: 17101
    working_directory: /tmp
    command: npm run dev
`, "")
	f.sync(Options{Herd: f.herd(testproc.FakeHerd{})})
	if err := os.MkdirAll(filepath.Join(f.configDir, "projects.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	hand := "# my file\nprojects:\n  hand:\n    public_url: https://hand.test\n    supervisor_port: 7102\n    application_port: 17102\n    working_directory: /tmp\n    command: npm run dev\n  other:\n    public_url: https://other.test\n    supervisor_port: 7103\n    application_port: 17103\n    working_directory: /tmp\n    command: npm run dev\n"
	if err := os.WriteFile(filepath.Join(f.configDir, "projects.d", "hand.yaml"), []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cli := f.herd(testproc.FakeHerd{})

	// A managed project: removed from the managed file, unproxied.
	res, err := Remove(ctx, f.load(), "issue-1", RemoveOptions{Herd: cli})
	if err != nil {
		t.Fatalf("Remove(issue-1): %v", err)
	}
	if !res.Managed || res.File != f.managedFile() || res.Source != "projects.d/webapp.yaml" {
		t.Errorf("result = %+v", res)
	}
	if projects := f.managed(); projects["issue-1"] != nil || projects["issue-2"] == nil {
		t.Errorf("managed projects after remove = %v", projects)
	}
	if data, _ := os.ReadFile(f.managedFile()); !strings.HasPrefix(string(data), managedMarker) {
		t.Error("managed header lost on rewrite")
	}
	if a := findHerd(t, res.Herd, "unproxy", "issue-1"); a.Status != HerdDone {
		t.Errorf("unproxy = %+v", a)
	}
	if calls := f.herdMutations(); calls[len(calls)-1] != "unproxy issue-1" {
		t.Errorf("herd calls = %v", calls)
	}

	// A hand-written projects.d project: removed from its file; the other
	// project in the file survives; keep-herd runs nothing.
	res, err = Remove(ctx, f.load(), "hand", RemoveOptions{Herd: cli, KeepHerd: true})
	if err != nil {
		t.Fatalf("Remove(hand): %v", err)
	}
	if res.Managed || len(res.Herd) != 0 {
		t.Errorf("result = %+v", res)
	}
	left, err := config.DecodeFragment(filepath.Join(f.configDir, "projects.d", "hand.yaml"))
	if err != nil || left["hand"] != nil || left["other"] == nil {
		t.Errorf("hand.yaml after remove = %v, %v", left, err)
	}

	// The main config is never rewritten.
	var mainErr *MainConfigError
	if _, err := Remove(ctx, f.load(), "dashboard", RemoveOptions{}); !errors.As(err, &mainErr) || mainErr.Name != "dashboard" {
		t.Errorf("Remove(dashboard) error = %v, want MainConfigError", err)
	}
	if _, err := Remove(ctx, f.load(), "nope", RemoveOptions{}); !errors.Is(err, ErrNoSuchProject) {
		t.Errorf("Remove(nope) error = %v, want ErrNoSuchProject", err)
	}

	// Without Herd, the manual command is reported.
	res, err = Remove(ctx, f.load(), "issue-2", RemoveOptions{HerdNote: "no herd"})
	if err != nil {
		t.Fatal(err)
	}
	if a := findHerd(t, res.Herd, "unproxy", "issue-2"); a.Status != HerdManual || a.Command != "herd unproxy issue-2" {
		t.Errorf("unproxy without herd = %+v", a)
	}
}

func TestRemoveLeavesForeignSiteFileAlone(t *testing.T) {
	f := newFixture(t)
	f.worktree("issue-1")
	f.writeConfig("", "")
	f.sync(Options{})
	cli := f.herd(testproc.FakeHerd{})
	if err := os.WriteFile(filepath.Join(f.nginxDir, "issue-1.test"), []byte("proxy_pass http://127.0.0.1:9999;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Remove(context.Background(), f.load(), "issue-1", RemoveOptions{Herd: cli})
	if err != nil {
		t.Fatal(err)
	}
	if a := findHerd(t, res.Herd, "unproxy", "issue-1"); a.Status != HerdSkipped || !strings.Contains(a.Detail, "left alone") {
		t.Errorf("unproxy of a foreign proxy = %+v, want skipped", a)
	}
	if got := f.herdMutations(); len(got) != 0 {
		t.Errorf("herd mutations = %v, want none", got)
	}
}
