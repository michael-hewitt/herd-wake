package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// projectYAML renders one valid project block (indented under projects:).
func projectYAML(name string, supervisorPort, applicationPort int) string {
	return fmt.Sprintf(`  %s:
    public_url: https://%s.test
    supervisor_port: %d
    application_port: %d
    working_directory: /tmp
    command: npm run dev
`, name, name, supervisorPort, applicationPort)
}

// writeTree writes a main config file plus projects.d files into a fresh
// directory and returns the main file's path. A nil main leaves the main
// file out entirely.
func writeTree(t *testing.T, main string, fragments map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(fragments) > 0 {
		if err := os.MkdirAll(filepath.Join(dir, ProjectsDirName), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range fragments {
		full := filepath.Join(dir, ProjectsDirName, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestProjectsDir(t *testing.T) {
	got := ProjectsDir("/Users/me/Library/Application Support/herd-wake/config.yaml")
	want := "/Users/me/Library/Application Support/herd-wake/projects.d"
	if got != want {
		t.Errorf("ProjectsDir() = %q, want %q", got, want)
	}
}

func TestLoadMergesProjectsDir(t *testing.T) {
	path := writeTree(t,
		"projects:\n"+projectYAML("alpha", 7101, 17101),
		map[string]string{
			"beta.yaml":     "projects:\n" + projectYAML("beta", 7102, 17102),
			"gamma.yaml":    "projects:\n" + projectYAML("gamma-one", 7103, 17103) + projectYAML("gamma-two", 7104, 17104),
			"empty.yaml":    "",
			"comments.yaml": "# nothing registered here yet\n",
			"null.yaml":     "projects:\n",
			"notes.txt":     "not yaml at all {{{",
			".hidden.yaml":  "garbage: [",
			"sub/deep.yaml": "projects:\n" + projectYAML("deep", 7199, 17199),
		})

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if got, want := strings.Join(cfg.ProjectNames(), " "), "alpha beta gamma-one gamma-two"; got != want {
		t.Fatalf("ProjectNames() = %q, want %q", got, want)
	}
	if abs, _ := filepath.Abs(path); cfg.Path != abs {
		t.Errorf("Path = %q, want %q", cfg.Path, abs)
	}
	for name, want := range map[string]string{
		"alpha":     "config.yaml",
		"beta":      "projects.d/beta.yaml",
		"gamma-one": "projects.d/gamma.yaml",
		"gamma-two": "projects.d/gamma.yaml",
	} {
		if got := cfg.Projects[name].Source; got != want {
			t.Errorf("%s.Source = %q, want %q", name, got, want)
		}
	}
	// Defaults apply to fragment projects exactly like main-file ones.
	beta := cfg.Projects["beta"]
	if beta.Name != "beta" || beta.ReadinessURL != "http://127.0.0.1:17102/" || beta.IdleTimeoutMinutes != DefaultIdleTimeoutMinutes {
		t.Errorf("fragment project not defaulted: %+v", beta)
	}
}

func TestLoadProjectsDirMissingIsFine(t *testing.T) {
	path := writeTree(t, "projects:\n"+projectYAML("alpha", 7101, 17101), nil)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if got := cfg.ProjectNames(); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("ProjectNames() = %v, want [alpha]", got)
	}
}

func TestLoadEmptyMainConfigWithProjectsDir(t *testing.T) {
	path := writeTree(t, "# all projects live in projects.d\n", map[string]string{
		"alpha.yaml": "projects:\n" + projectYAML("alpha", 7101, 17101),
	})

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if got := cfg.ProjectNames(); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("ProjectNames() = %v, want [alpha]", got)
	}
	if cfg.Projects["alpha"].Source != "projects.d/alpha.yaml" {
		t.Errorf("Source = %q", cfg.Projects["alpha"].Source)
	}
}

func TestLoadEmptyConfigHasNoProjects(t *testing.T) {
	path := writeTree(t, "", nil)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if len(cfg.Projects) != 0 {
		t.Errorf("Projects = %v, want none", cfg.Projects)
	}
}

func TestLoadProjectsDirDuplicateNamesRejected(t *testing.T) {
	path := writeTree(t,
		"projects:\n"+projectYAML("alpha", 7101, 17101),
		map[string]string{
			"alpha.yaml": "projects:\n" + projectYAML("alpha", 7102, 17102),
			"b1.yaml":    "projects:\n" + projectYAML("beta", 7103, 17103),
			"b2.yaml":    "projects:\n" + projectYAML("beta", 7104, 17104),
		})

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load should reject a project defined twice")
	}
	got := err.Error()
	wantError(t, got, "alpha", "name")
	wantError(t, got, "beta", "name")
	for _, want := range []string{
		`project "alpha": name: defined in both config.yaml and projects.d/alpha.yaml`,
		`project "beta": name: defined in both projects.d/b1.yaml and projects.d/b2.yaml`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("errors missing %q; got:\n%s", want, got)
		}
	}
}

func TestLoadProjectsDirRejectsUnknownTopLevelKey(t *testing.T) {
	path := writeTree(t, "projects:\n"+projectYAML("alpha", 7101, 17101), map[string]string{
		"beta.yaml": "settings:\n  foo: bar\nprojects:\n" + projectYAML("beta", 7102, 17102),
	})

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load should reject an unknown top-level key in a projects.d file")
	}
	if !strings.Contains(err.Error(), "settings") || !strings.Contains(err.Error(), filepath.Join("projects.d", "beta.yaml")) {
		t.Errorf("error = %v, want it to name the unknown key and the file", err)
	}
}

func TestLoadProjectsDirMalformedFragment(t *testing.T) {
	path := writeTree(t, "projects:\n"+projectYAML("alpha", 7101, 17101), map[string]string{
		"beta.yaml": "projects: [",
	})

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load should reject a malformed projects.d file")
	}
	if !strings.Contains(err.Error(), "parse config") || !strings.Contains(err.Error(), "beta.yaml") {
		t.Errorf("error = %v, want a parse error naming beta.yaml", err)
	}
}

func TestLoadProjectsDirValidatesAcrossFiles(t *testing.T) {
	// beta (fragment) reuses alpha's supervisor port and lacks a command:
	// both problems are reported, naming beta and the other claimant.
	path := writeTree(t, "projects:\n"+projectYAML("alpha", 7101, 17101), map[string]string{
		"beta.yaml": `projects:
  beta:
    public_url: https://beta.test
    supervisor_port: 7101
    application_port: 17102
    working_directory: /tmp
`,
	})

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load should report validation errors from projects.d files")
	}
	got := err.Error()
	wantError(t, got, "beta", "command")
	wantError(t, got, "beta", "supervisor_port")
	if !strings.Contains(got, `"alpha"`) {
		t.Errorf("port conflict should name alpha; got:\n%s", got)
	}
}

func TestProjectEquivalentIgnoresSource(t *testing.T) {
	base := func() *Project {
		keepAlive := true
		return &Project{
			Name:                "alpha",
			Source:              "config.yaml",
			PublicURL:           "https://alpha.test",
			SupervisorPort:      7101,
			ApplicationPort:     17101,
			WorkingDirectory:    "/tmp",
			Command:             "npm run dev",
			WebSocketsKeepAlive: &keepAlive,
			Env:                 map[string]string{"A": "1"},
		}
	}

	moved := base()
	moved.Source = "projects.d/alpha.yaml"
	if !base().Equivalent(moved) {
		t.Error("a project moved between files must count as unchanged")
	}

	changes := map[string]func(p *Project){
		"command":      func(p *Project) { p.Command = "pnpm dev" },
		"env":          func(p *Project) { p.Env["A"] = "2" },
		"rewrite_host": func(p *Project) { p.RewriteHost = true },
		"keep_alive":   func(p *Project) { off := false; p.WebSocketsKeepAlive = &off },
		"port":         func(p *Project) { p.SupervisorPort = 7102 },
		"always_on":    func(p *Project) { p.AlwaysOn = true },
	}
	for field, change := range changes {
		changed := base()
		change(changed)
		if base().Equivalent(changed) {
			t.Errorf("changing %s must count as a change", field)
		}
	}
}
