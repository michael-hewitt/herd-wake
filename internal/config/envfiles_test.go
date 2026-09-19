package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestLoadEnvFileForms: env_file accepts one path or a list of them, an
// absent list stays nil, and diagnostic_logs defaults to on.
func TestLoadEnvFileForms(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "env_file.yaml"))
	if err != nil {
		t.Fatalf("Load(env_file.yaml) error: %v", err)
	}

	if got := cfg.Projects["scalar"].EnvFile; len(got) != 1 || got[0] != ".env.preview" {
		t.Errorf("scalar EnvFile = %v, want [.env.preview]", got)
	}
	if got := cfg.Projects["listed"].EnvFile; len(got) != 2 || got[0] != "shared.env" || got[1] != ".env.preview" {
		t.Errorf("listed EnvFile = %v, want [shared.env .env.preview]", got)
	}
	if got := cfg.Projects["bare"].EnvFile; got != nil {
		t.Errorf("EnvFile without env_file = %v, want nil", got)
	}

	if !cfg.Projects["scalar"].DiagnosticLogsEnabled() || !cfg.Projects["bare"].DiagnosticLogsEnabled() {
		t.Error("DiagnosticLogsEnabled() = false, want true by default")
	}
	if cfg.Projects["listed"].DiagnosticLogsEnabled() {
		t.Error("listed.DiagnosticLogsEnabled() = true, want false (diagnostic_logs: false)")
	}
}

// TestLoadEnvFileExpandsHome: a leading ~/ becomes the home directory, so
// the absolute-path rules apply to the expanded path.
func TestLoadEnvFileExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, err := os.UserHomeDir(); err != nil || got != home {
		t.Skipf("home directory is not taken from HOME here (got %q, %v)", got, err)
	}
	secrets := filepath.Join(home, "secrets.env")
	if err := os.WriteFile(secrets, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(writeConfigFile(t, `projects:
  homey:
    public_url: https://homey.test
    supervisor_port: 7101
    application_port: 17101
    working_directory: /tmp
    command: npm run dev
    env_file: "~/secrets.env"
`))
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if got := cfg.Projects["homey"].EnvFile; len(got) != 1 || got[0] != secrets {
		t.Errorf("EnvFile = %v, want [%s]", got, secrets)
	}
}

// TestValidateEnvFileEntries: an absolute env file must exist and be a
// regular file, a blank entry is refused, and a relative entry is accepted
// without being looked for.
func TestValidateEnvFileEntries(t *testing.T) {
	got := loadInvalid(t, "env_file_bad.yaml")

	wantError(t, got, "leaky", "env_file")
	for _, want := range []string{
		`file "/nonexistent/herd-wake/preview.env" does not exist`,
		`"/tmp" is not a regular file`,
		"entries must not be empty",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("validation errors missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, ".env.preview") {
		t.Errorf("a relative env_file entry must not be checked; got:\n%s", got)
	}
}

// TestValidateEnvFileAcceptsExistingAbsolutePath: the happy path, and it
// still holds when the working-directory check is skipped (a repair tool
// wants to hear about vanished secrets too).
func TestValidateEnvFileAcceptsExistingAbsolutePath(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "preview.env")
	if err := os.WriteFile(shared, []byte("ENVIRONMENT=preview\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := func(file string) string {
		return fmt.Sprintf(`projects:
  preview:
    public_url: https://preview.test
    supervisor_port: 7101
    application_port: 17101
    working_directory: /tmp
    command: npm run dev
    env_file: [%s, .env.preview]
`, file)
	}

	if _, err := Load(writeConfigFile(t, body(shared))); err != nil {
		t.Fatalf("Load with an existing shared env file: %v", err)
	}

	gone := filepath.Join(t.TempDir(), "gone.env")
	_, err := LoadWithOptions(writeConfigFile(t, body(gone)), LoadOptions{SkipWorkingDirectoryCheck: true})
	if err == nil || !strings.Contains(err.Error(), `env_file: file "`+gone+`" does not exist`) {
		t.Errorf("LoadWithOptions(skip) error = %v, want the missing env file reported", err)
	}
}

// TestEnvFileRefs: absolute entries are required and taken as they stand,
// relative ones are joined onto the caller's directory and are optional.
func TestEnvFileRefs(t *testing.T) {
	shared := filepath.Join(string(filepath.Separator), "etc", "herd-wake", "preview.env")
	worktree := filepath.Join(string(filepath.Separator), "srv", "previews", "issue-1")
	p := &Project{EnvFile: EnvFiles{shared, ".env.preview"}}

	got := p.EnvFileRefs(worktree)
	want := []EnvFileRef{
		{Path: shared, Optional: false},
		{Path: filepath.Join(worktree, ".env.preview"), Optional: true},
	}
	if len(got) != len(want) {
		t.Fatalf("EnvFileRefs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("EnvFileRefs()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if refs := (&Project{}).EnvFileRefs(worktree); refs != nil {
		t.Errorf("EnvFileRefs() without env_file = %v, want nil", refs)
	}
}

// TestEnvFilesRoundTrip guards the shape managed files are written in: one
// entry stays a scalar, several become a sequence, and both read back as
// the list they were.
func TestEnvFilesRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files EnvFiles
		want  string
	}{
		{"one entry stays a scalar", EnvFiles{"/etc/herd-wake/preview.env"}, "env_file: /etc/herd-wake/preview.env\n"},
		{"two entries become a sequence", EnvFiles{"/etc/herd-wake/preview.env", ".env.preview"}, "env_file:\n        - /etc/herd-wake/preview.env\n        - .env.preview\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			off := false
			p := &Project{
				PublicURL:        "https://alpha.test",
				SupervisorPort:   41000,
				ApplicationPort:  42000,
				WorkingDirectory: "/tmp/alpha",
				Command:          "./start.sh",
				EnvFile:          tc.files,
				DiagnosticLogs:   &off,
			}
			out, err := yaml.Marshal(map[string]*Project{"alpha": p})
			if err != nil {
				t.Fatal(err)
			}
			text := string(out)
			if !strings.Contains(text, tc.want) {
				t.Errorf("marshalled project missing %q; got:\n%s", tc.want, text)
			}
			if !strings.Contains(text, "diagnostic_logs: false") {
				t.Errorf("marshalled project missing diagnostic_logs: false; got:\n%s", text)
			}

			var back map[string]*Project
			if err := yaml.Unmarshal(out, &back); err != nil {
				t.Fatal(err)
			}
			if !back["alpha"].Equivalent(p) {
				t.Errorf("round trip changed the project:\n%+v\nvs\n%+v", back["alpha"], p)
			}
		})
	}

	// An unset list is omitted entirely, and "empty" and "unset" describe
	// the same registration.
	out, err := yaml.Marshal(map[string]*Project{"alpha": {Command: "./start.sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "env_file") || strings.Contains(string(out), "diagnostic_logs") {
		t.Errorf("unset fields should be omitted; got:\n%s", out)
	}
	empty := &Project{Command: "./start.sh", EnvFile: EnvFiles{}}
	empty.applyDefaults()
	unset := &Project{Command: "./start.sh"}
	unset.applyDefaults()
	if !empty.Equivalent(unset) {
		t.Error("an empty env_file list should be equivalent to an unset one")
	}
}

// TestEnvFilesRejectsWrongShape: a mapping is neither a path nor a list.
func TestEnvFilesRejectsWrongShape(t *testing.T) {
	_, err := Load(writeConfigFile(t, `projects:
  odd:
    public_url: https://odd.test
    supervisor_port: 7101
    application_port: 17101
    working_directory: /tmp
    command: npm run dev
    env_file: { path: /etc/herd-wake/preview.env }
`))
	if err == nil || !strings.Contains(err.Error(), "env_file must be a path or a list of paths") {
		t.Errorf("Load error = %v, want a shape complaint about env_file", err)
	}
}
