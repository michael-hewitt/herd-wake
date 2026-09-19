package envfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michael-hewitt/herd-wake/internal/config"
)

// TestParse walks the documented dotenv subset rule by rule, and checks
// that every rejection names the line and quotes none of it.
func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Var
	}{
		{
			name:  "plain assignment",
			input: "ENVIRONMENT=staging\n",
			want:  []Var{{Name: "ENVIRONMENT", Value: "staging"}},
		},
		{
			name:  "no trailing newline",
			input: "ENVIRONMENT=staging",
			want:  []Var{{Name: "ENVIRONMENT", Value: "staging"}},
		},
		{
			name:  "blank lines and comments ignored",
			input: "\n  \n# a comment\n\t# indented comment\nA=1\n",
			want:  []Var{{Name: "A", Value: "1"}},
		},
		{
			name:  "export prefix stripped",
			input: "export A=1\nexport\tB=2\n",
			want:  []Var{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}},
		},
		{
			name:  "exported is an ordinary name",
			input: "exported=1\n",
			want:  []Var{{Name: "exported", Value: "1"}},
		},
		{
			name:  "unquoted value is trimmed but otherwise verbatim",
			input: "  A =   hello world  \n",
			want:  []Var{{Name: "A", Value: "hello world"}},
		},
		{
			name:  "hash inside an unquoted value is not a comment",
			input: "DB_LOGIN=pa#ss word\n",
			want:  []Var{{Name: "DB_LOGIN", Value: "pa#ss word"}},
		},
		{
			name:  "empty value",
			input: "A=\n",
			want:  []Var{{Name: "A", Value: ""}},
		},
		{
			name:  "single quotes are literal",
			input: `A='  $x \n "quoted" # '` + "\n",
			want:  []Var{{Name: "A", Value: `  $x \n "quoted" # `}},
		},
		{
			name:  "double quotes honour the three escapes",
			input: `A="line\none\ttab \"q\" \\ end"` + "\n",
			want:  []Var{{Name: "A", Value: "line\none\\ttab \"q\" \\ end"}},
		},
		{
			name:  "value with an equals sign",
			input: "DSN=postgres://u:p@h/db?a=b\n",
			want:  []Var{{Name: "DSN", Value: "postgres://u:p@h/db?a=b"}},
		},
		{
			name:  "crlf tolerated",
			input: "A=1\r\nB=2\r\n",
			want:  []Var{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}},
		},
		{
			name:  "bom skipped",
			input: "\uFEFFA=1\n",
			want:  []Var{{Name: "A", Value: "1"}},
		},
		{
			name:  "last duplicate wins",
			input: "A=1\nB=2\nA=3\n",
			want:  []Var{{Name: "B", Value: "2"}, {Name: "A", Value: "3"}},
		},
		{
			name:  "underscore and digit names",
			input: "_A=1\nB2_C=2\n",
			want:  []Var{{Name: "_A", Value: "1"}, {Name: "B2_C", Value: "2"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.input))
			if err != nil {
				t.Fatalf("Parse(%q) returned error %v, want none", tt.input, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Parse(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Parse(%q)[%d] = %+v, want %+v", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestParseErrors asserts both halves of the contract for a bad line: the
// error says which line it was, and says nothing about what was on it. The
// secret in every fixture is "hunter2".
func TestParseErrors(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLine string
		wantWhat string
	}{
		{
			name:     "no equals sign",
			input:    "A=1\nhunter2\n",
			wantLine: "line 2",
			wantWhat: "no '='",
		},
		{
			name:     "name is not an identifier",
			input:    "A-B=hunter2\n",
			wantLine: "line 1",
			wantWhat: "shell identifier",
		},
		{
			name:     "name starts with a digit",
			input:    "\n2FA=hunter2\n",
			wantLine: "line 2",
			wantWhat: "shell identifier",
		},
		{
			name:     "empty name",
			input:    "=hunter2\n",
			wantLine: "line 1",
			wantWhat: "shell identifier",
		},
		{
			name:     "unterminated single quote",
			input:    "A='hunter2\n",
			wantLine: "line 1",
			wantWhat: "unterminated single-quoted",
		},
		{
			name:     "unterminated double quote",
			input:    "# comment\nA=\"hunter2\n",
			wantLine: "line 2",
			wantWhat: "unterminated double-quoted",
		},
		{
			name:     "trailing backslash in a double-quoted value",
			input:    `A="hunter2\` + "\n",
			wantLine: "line 1",
			wantWhat: "unterminated double-quoted",
		},
		{
			name:     "text after the closing quote",
			input:    "A='x' hunter2\n",
			wantLine: "line 1",
			wantWhat: "trailing text after closing quote",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.input))
			if err == nil {
				t.Fatalf("Parse(%q) returned no error, want one", tt.input)
			}
			msg := err.Error()
			if !strings.Contains(msg, tt.wantLine) {
				t.Errorf("Parse error = %q, want it to contain %q", msg, tt.wantLine)
			}
			if !strings.Contains(msg, tt.wantWhat) {
				t.Errorf("Parse error = %q, want it to contain %q", msg, tt.wantWhat)
			}
			if strings.Contains(msg, "hunter2") {
				t.Errorf("Parse error = %q, want it to leak none of the line's content", msg)
			}
		})
	}
}

// writeEnvFile writes an env file with the given mode and returns its path.
func writeEnvFile(t *testing.T, dir, name, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	// WriteFile's mode is masked by the umask; force the mode we asked for.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	return path
}

// varsToMap reduces a variable list the way applying it in order would.
func varsToMap(vars []Var) map[string]string {
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Name] = v.Value
	}
	return m
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	shared := writeEnvFile(t, dir, "shared.env", "ENVIRONMENT=staging\nDB_LOGIN=shared\n", 0o600)
	worktree := writeEnvFile(t, dir, ".env.preview", "DB_LOGIN=branch\nSUPPRESS_DB_TESTS=1\n", 0o600)

	t.Run("required file present", func(t *testing.T) {
		vars, warnings, err := Load([]config.EnvFileRef{{Path: shared}})
		if err != nil {
			t.Fatalf("Load returned error %v, want none", err)
		}
		if len(warnings) != 0 {
			t.Errorf("Load warnings = %v, want none for a 0600 file", warnings)
		}
		if got, want := varsToMap(vars)["ENVIRONMENT"], "staging"; got != want {
			t.Errorf("ENVIRONMENT = %q, want %q", got, want)
		}
	})

	t.Run("required file missing", func(t *testing.T) {
		missing := filepath.Join(dir, "gone.env")
		_, _, err := Load([]config.EnvFileRef{{Path: missing}})
		if err == nil {
			t.Fatalf("Load of a missing required file returned no error, want one")
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("Load error = %q, want it to name %q", err, missing)
		}
	})

	t.Run("optional file missing is skipped", func(t *testing.T) {
		vars, _, err := Load([]config.EnvFileRef{
			{Path: shared},
			{Path: filepath.Join(dir, "absent.env"), Optional: true},
		})
		if err != nil {
			t.Fatalf("Load returned error %v, want a missing optional file to be skipped", err)
		}
		if got, want := len(vars), 2; got != want {
			t.Errorf("Load returned %d variables, want %d", got, want)
		}
	})

	t.Run("later file wins", func(t *testing.T) {
		vars, _, err := Load([]config.EnvFileRef{{Path: shared}, {Path: worktree, Optional: true}})
		if err != nil {
			t.Fatalf("Load returned error %v, want none", err)
		}
		got := varsToMap(vars)
		if got["DB_LOGIN"] != "branch" {
			t.Errorf("DB_LOGIN = %q, want %q (the later file wins)", got["DB_LOGIN"], "branch")
		}
		if got["ENVIRONMENT"] != "staging" {
			t.Errorf("ENVIRONMENT = %q, want %q (kept from the earlier file)", got["ENVIRONMENT"], "staging")
		}
		if got["SUPPRESS_DB_TESTS"] != "1" {
			t.Errorf("SUPPRESS_DB_TESTS = %q, want %q", got["SUPPRESS_DB_TESTS"], "1")
		}
	})

	t.Run("parse error names the file and the line", func(t *testing.T) {
		bad := writeEnvFile(t, dir, "bad.env", "A=1\nhunter2\n", 0o600)
		_, _, err := Load([]config.EnvFileRef{{Path: bad}})
		if err == nil {
			t.Fatalf("Load of a malformed file returned no error, want one")
		}
		msg := err.Error()
		for _, want := range []string{"env_file", bad, "line 2"} {
			if !strings.Contains(msg, want) {
				t.Errorf("Load error = %q, want it to contain %q", msg, want)
			}
		}
		if strings.Contains(msg, "hunter2") {
			t.Errorf("Load error = %q, want it to leak none of the file's content", msg)
		}
	})

	t.Run("world-readable file warns", func(t *testing.T) {
		loose := writeEnvFile(t, dir, "loose.env", "A=1\n", 0o644)
		_, warnings, err := Load([]config.EnvFileRef{{Path: loose}})
		if err != nil {
			t.Fatalf("Load returned error %v, want a permission problem to be a warning only", err)
		}
		if len(warnings) != 1 {
			t.Fatalf("Load warnings = %v, want exactly one", warnings)
		}
		for _, want := range []string{loose, "0644", "chmod 600"} {
			if !strings.Contains(warnings[0], want) {
				t.Errorf("warning = %q, want it to contain %q", warnings[0], want)
			}
		}
	})

	t.Run("group-readable file warns", func(t *testing.T) {
		group := writeEnvFile(t, dir, "group.env", "A=1\n", 0o640)
		_, warnings, err := Load([]config.EnvFileRef{{Path: group}})
		if err != nil {
			t.Fatalf("Load returned error %v, want none", err)
		}
		if len(warnings) != 1 {
			t.Fatalf("Load warnings = %v, want exactly one", warnings)
		}
	})
}

// envToMap reduces an environment slice the way os/exec does: the last
// occurrence of a name is the one the child sees.
func envToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		m[name] = value
	}
	return m
}

func TestChildEnv(t *testing.T) {
	dir := t.TempDir()
	nodeDir := filepath.Join(dir, "node", "bin")
	if err := os.MkdirAll(nodeDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", nodeDir, err)
	}
	worktree := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("creating %s: %v", worktree, err)
	}
	shared := writeEnvFile(t, dir, "shared.env", "ENVIRONMENT=staging\nDB_LOGIN=shared\nINLINE_WINS=file\n", 0o600)
	writeEnvFile(t, worktree, ".env.preview", "DB_LOGIN=branch\n", 0o600)

	t.Setenv("HW_INHERITED", "from-daemon")
	t.Setenv("ENVIRONMENT", "from-daemon")

	p := &config.Project{
		Name:     "preview",
		Env:      map[string]string{"INLINE_WINS": "inline", "PORT": "3000"},
		EnvFile:  config.EnvFiles{shared, ".env.preview"},
		NodePath: nodeDir,
	}

	env, warnings, err := ChildEnv(p, worktree)
	if err != nil {
		t.Fatalf("ChildEnv returned error %v, want none", err)
	}
	if len(warnings) != 0 {
		t.Errorf("ChildEnv warnings = %v, want none for 0600 files", warnings)
	}
	got := envToMap(env)

	checks := []struct{ name, want, why string }{
		{"HW_INHERITED", "from-daemon", "inherited from the daemon"},
		{"ENVIRONMENT", "staging", "an env file beats the inherited environment"},
		{"DB_LOGIN", "branch", "the per-worktree file beats the shared one"},
		{"INLINE_WINS", "inline", "the inline env map beats every file"},
		{"PORT", "3000", "from the inline env map"},
	}
	for _, c := range checks {
		if got[c.name] != c.want {
			t.Errorf("child %s = %q, want %q (%s)", c.name, got[c.name], c.want, c.why)
		}
	}
	if want := nodeDir + string(os.PathListSeparator); !strings.HasPrefix(got["PATH"], want) {
		t.Errorf("child PATH = %q, want it to start with %q", got["PATH"], want)
	}

	t.Run("missing optional file is fine", func(t *testing.T) {
		bare := filepath.Join(dir, "bare")
		if err := os.MkdirAll(bare, 0o755); err != nil {
			t.Fatalf("creating %s: %v", bare, err)
		}
		env, _, err := ChildEnv(p, bare)
		if err != nil {
			t.Fatalf("ChildEnv returned error %v, want a worktree without .env.preview to start", err)
		}
		if got := envToMap(env)["DB_LOGIN"]; got != "shared" {
			t.Errorf("child DB_LOGIN = %q, want %q (only the shared file applied)", got, "shared")
		}
	})

	t.Run("missing required file fails", func(t *testing.T) {
		missing := filepath.Join(dir, "gone.env")
		broken := &config.Project{Name: "preview", EnvFile: config.EnvFiles{missing}}
		if _, _, err := ChildEnv(broken, worktree); err == nil {
			t.Fatalf("ChildEnv with a missing shared env file returned no error, want one")
		} else if !strings.Contains(err.Error(), missing) {
			t.Errorf("ChildEnv error = %q, want it to name %q", err, missing)
		}
	})

	t.Run("no env files leaves the environment inherited", func(t *testing.T) {
		plain := &config.Project{Name: "plain"}
		env, warnings, err := ChildEnv(plain, worktree)
		if err != nil {
			t.Fatalf("ChildEnv returned error %v, want none", err)
		}
		if len(warnings) != 0 {
			t.Errorf("ChildEnv warnings = %v, want none", warnings)
		}
		if got := envToMap(env)["ENVIRONMENT"]; got != "from-daemon" {
			t.Errorf("child ENVIRONMENT = %q, want %q", got, "from-daemon")
		}
	})
}

func TestSensitive(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"DB_PASSWORD", true},
		{"password", true},
		{"API_SECRET", true},
		{"GITHUB_TOKEN", true},
		{"MY_API_KEY_2", true},
		{"DB_LOGIN", true},
		{"SENTRY_DSN", true},
		{"databaseUrl", true},
		{"PUBLIC_URL", true}, // deliberate over-match
		{"ENVIRONMENT", false},
		{"PORT", false},
		{"SUPPRESS_DB_TESTS", false},
		{"NODE_ENV", false},
	}
	for _, tt := range tests {
		if got := Sensitive(tt.name); got != tt.want {
			t.Errorf("Sensitive(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDisplay(t *testing.T) {
	if got := Display("ENVIRONMENT", "staging"); got != "staging" {
		t.Errorf("Display(ENVIRONMENT) = %q, want %q", got, "staging")
	}
	if got := Display("DB_LOGIN", "hunter2"); got != Redacted {
		t.Errorf("Display(DB_LOGIN) = %q, want %q", got, Redacted)
	}
	if got := Display("db_password", "hunter2"); got != Redacted {
		t.Errorf("Display(db_password) = %q, want %q", got, Redacted)
	}
}
