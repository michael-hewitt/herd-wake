package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

// writeWildcardConfig writes the main config with one wildcard entry (plus
// extra entry lines) and the given projects body.
func (f *fixture) writeWildcardConfig(projects, extra string) {
	f.t.Helper()
	if projects == "" {
		projects = " {}\n"
	} else {
		projects = "\n" + projects
	}
	body := fmt.Sprintf(`projects:%sdiscovery:
  - name: webapp
    mode: wildcard
    supervisor_port: 41000
    directory: %s
    repository: %s
    command: ./start.sh
    application_port_range: [42000, 42999]
%s`, projects, f.workspace, f.repo, extra)
	if err := os.WriteFile(f.configPath, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) statePath() string { return StatePath(f.configPath) }

func (f *fixture) syncAll(opts Options) *Result {
	f.t.Helper()
	res, err := Sync(context.Background(), f.load(), opts)
	if err != nil {
		f.t.Fatalf("Sync: %v", err)
	}
	return res
}

// TestSyncWildcardProxiesBaseNameOnce: a wildcard entry makes sync do one
// thing for Herd — proxy the base name to the shared port — and write no
// projects.d file; a second sync runs no Herd command at all.
func TestSyncWildcardProxiesBaseNameOnce(t *testing.T) {
	f := newFixture(t)
	f.worktree("a")
	f.worktree("b")
	f.writeWildcardConfig("", "")
	cli := f.herd(testproc.FakeHerd{})

	res := f.sync(Options{Herd: cli})
	if len(res.Errors) > 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
	if res.Mode != config.ModeWildcard || res.BaseDomain != "webapp.test" || res.SupervisorPort != 41000 || res.URLPattern != "https://<label>.webapp.test" || res.File != "" {
		t.Errorf("entry result = %+v", res)
	}
	if len(res.Added)+len(res.Updated)+len(res.Unchanged)+len(res.Removed)+len(res.Skipped) != 0 || res.Changed || res.Written {
		t.Errorf("a wildcard entry should register no projects: %+v", res)
	}
	if len(res.Herd) != 1 || res.Herd[0].Action != "proxy" || res.Herd[0].Site != "webapp" || res.Herd[0].Port != 41000 || res.Herd[0].Status != HerdDone {
		t.Errorf("herd actions = %+v", res.Herd)
	}
	if got := f.herdMutations(); len(got) != 1 || got[0] != "proxy webapp http://127.0.0.1:41000 --secure" {
		t.Errorf("herd mutations = %v", got)
	}
	if _, err := os.Stat(filepath.Join(f.configDir, "projects.d")); !os.IsNotExist(err) {
		t.Error("sync created projects.d for a wildcard entry")
	}
	if _, err := os.Stat(f.statePath()); err != nil {
		t.Errorf("state file not written: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(f.nginxDir, "webapp.test"))
	if !strings.Contains(string(data), "*.webapp.test") {
		t.Errorf("Herd's site file should cover *.webapp.test:\n%s", data)
	}
	if _, err := config.Load(f.configPath); err != nil {
		t.Errorf("config no longer loads: %v", err)
	}

	calls := len(f.herdCalls())
	res = f.sync(Options{Herd: f.herd(testproc.FakeHerd{})})
	if len(res.Errors) > 0 || len(res.Herd) != 1 || res.Herd[0].Status != HerdSkipped || !strings.Contains(res.Herd[0].Detail, "already proxied") {
		t.Errorf("second sync = %+v", res)
	}
	if got := len(f.herdCalls()); got != calls {
		t.Errorf("second sync ran %d herd command(s), want none: %v", got-calls, f.herdCalls()[calls:])
	}
}

// TestSyncWildcardRepointsRefusesAndReports: a site file proxying
// elsewhere is re-pointed; a non-proxy site file or a Herd site of the
// base name is refused; herd: false and a missing CLI report the manual
// command; a dry run changes nothing.
func TestSyncWildcardRepointsRefusesAndReports(t *testing.T) {
	f := newFixture(t)
	f.writeWildcardConfig("", "")
	site := func(content string) {
		if err := os.MkdirAll(f.nginxDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.nginxDir, "webapp.test"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Dry run: nothing written, nothing mutated.
	res := f.sync(Options{Herd: f.herd(testproc.FakeHerd{}), DryRun: true})
	if res.Herd[0].Status != HerdDryRun || len(f.herdMutations()) != 0 {
		t.Errorf("dry run = %+v, mutations %v", res.Herd, f.herdMutations())
	}
	if _, err := os.Stat(f.statePath()); !os.IsNotExist(err) {
		t.Error("dry run wrote the state file")
	}

	// Re-point.
	site("location / {\n  proxy_pass http://127.0.0.1:7777;\n}\n")
	res = f.sync(Options{Herd: f.herd(testproc.FakeHerd{})})
	if a := res.Herd[0]; a.Status != HerdDone || !strings.Contains(a.Detail, "re-pointed from 127.0.0.1:7777") {
		t.Errorf("re-point = %+v", a)
	}

	// A secured PHP site of that name: refused.
	site("server {\n  root /x;\n}\n")
	res = f.sync(Options{Herd: f.herd(testproc.FakeHerd{})})
	if a := res.Herd[0]; a.Status != HerdRefused || len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "not a proxy") {
		t.Errorf("secured site = %+v, errors %v", a, res.Errors)
	}
	if err := os.Remove(filepath.Join(f.nginxDir, "webapp.test")); err != nil {
		t.Fatal(err)
	}

	// A parked directory of that name: refused, no proxy.
	if err := os.MkdirAll(filepath.Join(f.parked, "webapp"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := len(f.herdMutations())
	res = f.sync(Options{Herd: f.herd(testproc.FakeHerd{})})
	if a := res.Herd[0]; a.Status != HerdRefused || !strings.Contains(a.Detail, "parked path") || len(res.Errors) != 1 {
		t.Errorf("parked collision = %+v, errors %v", a, res.Errors)
	}
	if len(f.herdMutations()) != before {
		t.Error("a refused entry ran a herd mutation")
	}
	if err := os.RemoveAll(filepath.Join(f.parked, "webapp")); err != nil {
		t.Fatal(err)
	}

	// herd: false and no CLI: manual.
	f.writeWildcardConfig("", "    herd: false\n")
	res = f.sync(Options{Herd: f.herd(testproc.FakeHerd{})})
	if a := res.Herd[0]; a.Status != HerdManual || a.Command != "herd proxy webapp http://127.0.0.1:41000 --secure" {
		t.Errorf("herd: false = %+v", a)
	}
	f.writeWildcardConfig("", "")
	all := f.syncAll(Options{HerdNote: "herd CLI not found"})
	if a := all.Entries[0].Herd[0]; a.Status != HerdManual || a.Detail != "herd CLI not found" {
		t.Errorf("no herd = %+v", a)
	}
	if got := all.ManualCommands(); len(got) != 1 || got[0] != "herd proxy webapp http://127.0.0.1:41000 --secure" {
		t.Errorf("ManualCommands = %v", got)
	}
}

// TestSyncWildcardStaleManagedFileNotice: a projects.d file left over from
// proxy mode is reported, never deleted or rewritten.
func TestSyncWildcardStaleManagedFileNotice(t *testing.T) {
	f := newFixture(t)
	f.worktree("a")
	// Proxy mode first, allocating from a range clear of the wildcard port.
	body := fmt.Sprintf("projects: {}\ndiscovery:\n  - name: webapp\n    directory: %s\n    repository: %s\n    supervisor_port_range: [41100, 41199]\n    application_port_range: [42000, 42999]\n    command: ./start.sh\n", f.workspace, f.repo)
	if err := os.WriteFile(f.configPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f.sync(Options{})
	stale, _ := os.ReadFile(f.managedFile())

	f.writeWildcardConfig("", "")
	res := f.sync(Options{Herd: f.herd(testproc.FakeHerd{})})
	if len(res.Notices) != 1 || !strings.Contains(res.Notices[0], f.managedFile()) || !strings.Contains(res.Notices[0], "delete it") {
		t.Errorf("notices = %v", res.Notices)
	}
	if now, _ := os.ReadFile(f.managedFile()); string(now) != string(stale) {
		t.Error("the stale managed file was modified")
	}
}

// TestSyncWildcardRemovedEntryUnproxies: sync remembers the proxy it
// registered; once the entry is gone (or its base_domain changed) the next
// sync removes the proxy and forgets it.
func TestSyncWildcardRemovedEntryUnproxies(t *testing.T) {
	f := newFixture(t)
	f.writeWildcardConfig("", "")
	f.sync(Options{Herd: f.herd(testproc.FakeHerd{})})

	// base_domain change: old site unproxied, new one proxied.
	f.writeWildcardConfig("", "    base_domain: preview.test\n")
	all := f.syncAll(Options{Herd: f.herd(testproc.FakeHerd{})})
	if len(all.Entries) != 2 {
		t.Fatalf("entries = %+v, want the live entry and the retired registration", all.Entries)
	}
	live, retired := all.Entries[0], all.Entries[1]
	if live.BaseDomain != "preview.test" || live.Herd[0].Status != HerdDone || live.Herd[0].Site != "preview" {
		t.Errorf("live entry = %+v", live)
	}
	if !retired.Retired || retired.Name != "webapp" || retired.BaseDomain != "webapp.test" || retired.Herd[0].Action != "unproxy" || retired.Herd[0].Status != HerdDone {
		t.Errorf("retired entry = %+v", retired)
	}
	want := []string{
		"proxy webapp http://127.0.0.1:41000 --secure",
		"proxy preview http://127.0.0.1:41000 --secure",
		"unproxy webapp",
	}
	if got := f.herdMutations(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("herd mutations = %v, want %v", got, want)
	}

	// Entry removed: unproxied, state file gone. A foreign site file (not
	// proxying to our port) is left alone.
	if err := os.WriteFile(f.configPath, []byte("projects: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	all = f.syncAll(Options{Herd: f.herd(testproc.FakeHerd{})})
	if len(all.Entries) != 1 || !all.Entries[0].Retired || all.Entries[0].Herd[0].Status != HerdDone {
		t.Errorf("after removal = %+v", all.Entries)
	}
	if calls := f.herdMutations(); calls[len(calls)-1] != "unproxy preview" {
		t.Errorf("herd mutations = %v, want unproxy preview last", calls)
	}
	if _, err := os.Stat(f.statePath()); !os.IsNotExist(err) {
		t.Error("state file should be removed once nothing is registered")
	}
	// Nothing left: a further sync is silent.
	all = f.syncAll(Options{Herd: f.herd(testproc.FakeHerd{})})
	if len(all.Entries) != 0 {
		t.Errorf("entries after everything was retired = %+v", all.Entries)
	}
}

// TestSyncWildcardReservesItsPortFromAllocation: a proxy-mode entry never
// allocates a wildcard entry's supervisor_port.
func TestSyncWildcardReservesItsPortFromAllocation(t *testing.T) {
	f := newFixture(t)
	f.worktree("a")
	body := fmt.Sprintf(`projects: {}
discovery:
  - name: wild
    mode: wildcard
    supervisor_port: 41000
    directory: %s
    command: ./start.sh
    application_port_range: [43000, 43999]
  - name: webapp
    directory: %s
    repository: %s
    supervisor_port_range: [41000, 41999]
    application_port_range: [42000, 42999]
    command: ./start.sh
`, t.TempDir(), f.workspace, f.repo)
	if err := os.WriteFile(f.configPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	all := f.syncAll(Options{})
	if p := f.managed()["a"]; p == nil || p.SupervisorPort != 41001 {
		t.Errorf("a should skip the wildcard port 41000: %+v (entries %+v)", p, all.Entries)
	}
}

// TestCheck applies the candidate rules to one label, reporting why it
// fails.
func TestCheck(t *testing.T) {
	f := newFixture(t)
	f.worktree("good", "start.sh")
	f.worktree("no-start")
	f.worktree("tmp-scratch", "start.sh")
	f.standaloneRepo("data-repo")
	other := t.TempDir()
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
	if err := os.MkdirAll(filepath.Join(f.workspace, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.workspace, "a-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.writeConfig("", "    require_files: [start.sh]\n    exclude: [\".*\", \"tmp-*\"]\n")
	d := f.load().Discovery[0]

	if dir, reason := Check(d, "good"); reason != "" || dir != filepath.Join(f.workspace, "good") {
		t.Errorf("Check(good) = %q, %q", dir, reason)
	}
	for label, want := range map[string]string{
		"no-start":    "missing required file(s): start.sh",
		"tmp-scratch": "excluded",
		"data-repo":   "standalone git repository",
		"foreign":     "different repository",
		"notes":       "not a git worktree",
		"a-file":      "is not a directory",
		"missing":     `no directory named "missing"`,
		"Bad_Name":    "not a valid DNS label",
		"../etc":      "not a valid DNS label",
	} {
		if _, reason := Check(d, label); !strings.Contains(reason, want) {
			t.Errorf("Check(%q) reason = %q, want %q", label, reason, want)
		}
	}
}
