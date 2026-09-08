package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// writeDiscoveryConfig writes a main config with the given discovery: body
// (indented under the list) into a fresh directory and returns its path.
func writeDiscoveryConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("discovery:\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeRepo creates a directory with a .git subdirectory.
func fakeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadDiscoveryEntry(t *testing.T) {
	workspace := t.TempDir()
	repo := fakeRepo(t)
	path := writeDiscoveryConfig(t, `  - name: webapp
    directory: `+workspace+`
    repository: `+repo+`
    require_files: [start.sh, package.json]
    url_template: https://{name}.dev.test
    command: ./start.sh
    port_command: node scripts/port.mjs
    application_port_range: [42000, 42999]
    supervisor_port_range: [41000, 41999]
    env: { ENVIRONMENT: dev }
    node_path: /opt/node/bin
    rewrite_host: true
    readiness_strategy: tcp
    startup_timeout_seconds: 180
    idle_timeout_minutes: 30
    websockets_keep_alive: false
    always_on: true
    herd: false
    exclude: [".*", "node_modules"]
  - name: minimal
    directory: `+workspace+`
    command: npm run dev
    supervisor_port_range: [7100, 7199]
    application_port_range: [17100, 17199]
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if len(cfg.Discovery) != 2 {
		t.Fatalf("Discovery = %d entries, want 2", len(cfg.Discovery))
	}
	d := cfg.Discovery[0]
	if d.Name != "webapp" || d.Directory != workspace || d.Repository != repo {
		t.Errorf("identity fields = %q/%q/%q", d.Name, d.Directory, d.Repository)
	}
	if got := strings.Join(d.RequireFiles, ","); got != "start.sh,package.json" {
		t.Errorf("RequireFiles = %q", got)
	}
	if d.URLTemplate != "https://{name}.dev.test" || d.PublicURL("issue-1") != "https://issue-1.dev.test" {
		t.Errorf("URLTemplate = %q, PublicURL = %q", d.URLTemplate, d.PublicURL("issue-1"))
	}
	if d.PortCommand != "node scripts/port.mjs" {
		t.Errorf("PortCommand = %q", d.PortCommand)
	}
	if len(d.ApplicationPortRange) != 2 || d.ApplicationPortRange[0] != 42000 || d.SupervisorPortRange[1] != 41999 {
		t.Errorf("ranges = %v / %v", d.ApplicationPortRange, d.SupervisorPortRange)
	}
	if d.HerdEnabled() {
		t.Error("HerdEnabled() = true, want false (herd: false)")
	}
	if !d.Excluded(".hidden") || !d.Excluded("node_modules") || d.Excluded("issue-1") {
		t.Errorf("Excluded() wrong for patterns %v", d.Exclude)
	}
	tpl := d.Template
	if tpl.Command != "./start.sh" || tpl.Env["ENVIRONMENT"] != "dev" || tpl.NodePath != "/opt/node/bin" || !tpl.RewriteHost {
		t.Errorf("template basics = %+v", tpl)
	}
	if tpl.ReadinessStrategy != ReadinessTCP || tpl.StartupTimeoutSeconds != 180 || tpl.IdleTimeoutMinutes != 30 || !tpl.AlwaysOn {
		t.Errorf("template options = %+v", tpl)
	}
	if tpl.WebSocketsKeepAlive == nil || *tpl.WebSocketsKeepAlive {
		t.Error("template websockets_keep_alive should be an explicit false")
	}
	// The template stays raw: no defaults are pinned into it.
	if tpl.ShutdownSignal != "" || tpl.ListenHost != "" || tpl.LogRetentionDays != 0 {
		t.Errorf("template should not carry defaults: %+v", tpl)
	}
	if d.FileName() != "webapp.yaml" || d.Source() != "projects.d/webapp.yaml" {
		t.Errorf("FileName/Source = %q/%q", d.FileName(), d.Source())
	}
	if want := filepath.Join(filepath.Dir(path), "projects.d", "webapp.yaml"); d.ManagedPath(path) != want {
		t.Errorf("ManagedPath = %q, want %q", d.ManagedPath(path), want)
	}

	// Defaults on the minimal entry.
	m := cfg.Discovery[1]
	if m.URLTemplate != DefaultURLTemplate || m.PublicURL("x") != "https://x.test" {
		t.Errorf("default URLTemplate = %q", m.URLTemplate)
	}
	if !m.HerdEnabled() {
		t.Error("HerdEnabled() should default to true")
	}
	if !m.Excluded(".git") || m.Excluded("node_modules") {
		t.Errorf("default Exclude = %v, want dot-dirs only", m.Exclude)
	}
	if m.Repository != "" || m.PortCommand != "" || len(m.RequireFiles) != 0 {
		t.Errorf("optional fields should stay empty: %+v", m)
	}
}

func TestLoadDiscoveryExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	path := writeDiscoveryConfig(t, `  - name: home
    directory: "~"
    command: npm run dev
    supervisor_port_range: [7100, 7199]
    application_port_range: [17100, 17199]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if got := cfg.Discovery[0].Directory; got != home {
		t.Errorf("Directory = %q, want home dir %q", got, home)
	}
}

func TestValidateDiscoveryReportsEveryProblem(t *testing.T) {
	workspace := t.TempDir()
	path := writeDiscoveryConfig(t, `  - name: "Bad_Name"
    directory: /nonexistent/herd-wake/workspace
    repository: /nonexistent/herd-wake/repo
    require_files: ["", /abs/file]
    url_template: https://fixed.test
    supervisor_port_range: [41000]
    application_port_range: [70000, 42999]
    exclude: ["[bad"]
    public_url: https://x.test
    supervisor_port: 1
    application_port: 2
    working_directory: /tmp
    shutdown_signal: SIGFOO
    idle_timeout_minutes: -1
  - directory: `+workspace+`
    command: npm run dev
  - name: dupe
    directory: `+workspace+`
    command: npm run dev
    supervisor_port_range: [41000, 41999]
    application_port_range: [42000, 41000]
  - name: dupe
    directory: `+workspace+`
    command: npm run dev
    supervisor_port_range: [41000, 41999]
    port_command: echo 42000
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load should reject the invalid discovery entries")
	}
	got := err.Error()
	wantDiscoveryError := func(entry, field string) {
		t.Helper()
		needle := `discovery "` + entry + `": ` + field + `:`
		if !strings.Contains(got, needle) {
			t.Errorf("errors missing %q; got:\n%s", needle, got)
		}
	}
	for _, field := range []string{
		"name", "directory", "repository", "require_files", "url_template",
		"supervisor_port_range", "application_port_range", "exclude",
		"public_url", "supervisor_port", "application_port", "working_directory",
		"shutdown_signal", "idle_timeout_minutes",
	} {
		wantDiscoveryError("Bad_Name", field)
	}
	// Unnamed entries are reported by position; a missing command is a
	// template error; the range is required without port_command.
	wantDiscoveryError("#2", "name")
	wantDiscoveryError("#2", "supervisor_port_range")
	wantDiscoveryError("#2", "application_port_range")
	wantDiscoveryError("dupe", "name")
	wantDiscoveryError("dupe", "application_port_range")
	if !strings.Contains(got, "must contain {name}") {
		t.Errorf("url_template error should mention {name}; got:\n%s", got)
	}
	if !strings.Contains(got, "not allowed in a discovery template") {
		t.Errorf("generated fields should be rejected in the template; got:\n%s", got)
	}
	// An entry with port_command needs no application_port_range: the
	// fourth entry's only problem is its duplicate name.
	if strings.Count(got, `discovery "dupe": application_port_range`) != 1 {
		t.Errorf("application_port_range should be optional with port_command; got:\n%s", got)
	}
	// Ordinary projects are still validated alongside.
	if strings.Contains(got, `project "`) {
		t.Errorf("no project errors expected; got:\n%s", got)
	}
}

func TestValidateDiscoveryMissingCommand(t *testing.T) {
	path := writeDiscoveryConfig(t, `  - name: nocmd
    directory: `+t.TempDir()+`
    supervisor_port_range: [41000, 41999]
    application_port_range: [42000, 42999]
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `discovery "nocmd": command: required`) {
		t.Errorf("Load error = %v, want a required-command error", err)
	}
}

func TestLoadRejectsUnknownDiscoveryField(t *testing.T) {
	path := writeDiscoveryConfig(t, `  - name: typo
    directory: `+t.TempDir()+`
    command: npm run dev
    supervisor_port_range: [41000, 41999]
    application_port_range: [42000, 42999]
    require_file: [start.sh]
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "require_file") {
		t.Errorf("Load error = %v, want the unknown field require_file named", err)
	}
}

func TestLoadRejectsDiscoveryInProjectsDir(t *testing.T) {
	path := writeTree(t, "projects:\n"+projectYAML("alpha", 7101, 17101), map[string]string{
		"disc.yaml": "discovery:\n  - name: x\n    directory: /tmp\n    command: npm run dev\n    supervisor_port_range: [1, 2]\n    application_port_range: [3, 4]\n",
	})
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "discovery entries belong in the main config file") {
		t.Errorf("Load error = %v, want discovery-in-projects.d rejection", err)
	}
}

func TestLoadSkipWorkingDirectoryCheck(t *testing.T) {
	path := writeTree(t, "projects:\n", map[string]string{
		"gone.yaml": `projects:
  gone:
    public_url: https://gone.test
    supervisor_port: 7101
    application_port: 17101
    working_directory: /nonexistent/herd-wake/gone
    command: npm run dev
`,
	})
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "working_directory") {
		t.Errorf("Load should reject the missing directory; got %v", err)
	}
	cfg, err := LoadWithOptions(path, LoadOptions{SkipWorkingDirectoryCheck: true})
	if err != nil {
		t.Fatalf("LoadWithOptions(skip) error: %v", err)
	}
	if cfg.Projects["gone"].Source != "projects.d/gone.yaml" {
		t.Errorf("Source = %q", cfg.Projects["gone"].Source)
	}
}

func TestDecodeFragmentIsRaw(t *testing.T) {
	path := writeTree(t, "projects:\n", map[string]string{
		"raw.yaml": "# a comment\nprojects:\n" + projectYAML("raw", 7101, 17101) + "  empty:\n",
	})
	projects, err := DecodeFragment(filepath.Join(filepath.Dir(path), ProjectsDirName, "raw.yaml"))
	if err != nil {
		t.Fatalf("DecodeFragment error: %v", err)
	}
	raw := projects["raw"]
	if raw == nil || raw.Name != "raw" || raw.SupervisorPort != 7101 {
		t.Fatalf("raw project = %+v", raw)
	}
	if raw.IdleTimeoutMinutes != 0 || raw.ReadinessStrategy != "" || raw.WebSocketsKeepAlive != nil {
		t.Errorf("DecodeFragment applied defaults: %+v", raw)
	}
	if projects["empty"] == nil || projects["empty"].Name != "empty" {
		t.Errorf("a null project should decode to an empty Project: %+v", projects["empty"])
	}
	if _, err := DecodeFragment(filepath.Join(t.TempDir(), "missing.yaml")); !os.IsNotExist(err) && !strings.Contains(err.Error(), "no such file") {
		t.Errorf("missing file error = %v, want not-exist", err)
	}
}

// TestProjectMarshalOmitsUnsetFields guards the omitempty tags: a raw
// project written back to YAML lists only what was set, and reads back the
// same.
func TestProjectMarshalOmitsUnsetFields(t *testing.T) {
	off := false
	p := &Project{
		PublicURL:           "https://alpha.test",
		SupervisorPort:      41000,
		ApplicationPort:     42000,
		WorkingDirectory:    "/tmp/alpha",
		Command:             "./start.sh",
		Env:                 map[string]string{"A": "1"},
		WebSocketsKeepAlive: &off,
		RewriteHost:         true,
	}
	out, err := yaml.Marshal(map[string]*Project{"alpha": p})
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, unwanted := range []string{"readiness_strategy", "startup_timeout_seconds", "idle_timeout_minutes", "shutdown_signal", "listen_host", "always_on", "node_path", "log_retention_days"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("marshalled project should omit unset %s; got:\n%s", unwanted, text)
		}
	}
	for _, wanted := range []string{"public_url: https://alpha.test", "supervisor_port: 41000", "websockets_keep_alive: false", "rewrite_host: true", "A: \"1\""} {
		if !strings.Contains(text, wanted) {
			t.Errorf("marshalled project missing %q; got:\n%s", wanted, text)
		}
	}
	var back map[string]*Project
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if !back["alpha"].Equivalent(p) {
		t.Errorf("round trip changed the project:\n%+v\nvs\n%+v", back["alpha"], p)
	}
}

func TestIsDNSLabel(t *testing.T) {
	for name, want := range map[string]bool{
		"issue-1":               true,
		"a":                     true,
		"abc123":                true,
		"Issue-1":               false,
		"issue_1":               false,
		"-issue":                false,
		"issue-":                false,
		"":                      false,
		"a.b":                   false,
		strings.Repeat("a", 63): true,
		strings.Repeat("a", 64): false,
	} {
		if got := IsDNSLabel(name); got != want {
			t.Errorf("IsDNSLabel(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestFieldErrorScope(t *testing.T) {
	if got := (&FieldError{Project: "x", Field: "f", Message: "m"}).Error(); got != `project "x": f: m` {
		t.Errorf("default scope = %q", got)
	}
	if got := (&FieldError{Scope: ScopeDiscovery, Project: "x", Field: "f", Message: "m"}).Error(); got != `discovery "x": f: m` {
		t.Errorf("discovery scope = %q", got)
	}
}
