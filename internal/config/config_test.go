package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadValid(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "valid.yaml"))
	if err != nil {
		t.Fatalf("Load(valid.yaml) error: %v", err)
	}
	if got := cfg.ProjectNames(); len(got) != 2 || got[0] != "dashboard" || got[1] != "minimal" {
		t.Fatalf("ProjectNames() = %v, want [dashboard minimal]", got)
	}

	dash := cfg.Projects["dashboard"]
	if dash.Name != "dashboard" {
		t.Errorf("Name = %q, want %q", dash.Name, "dashboard")
	}
	if dash.PublicURL != "https://dashboard.test" {
		t.Errorf("PublicURL = %q, want %q", dash.PublicURL, "https://dashboard.test")
	}
	if dash.SupervisorPort != 7101 || dash.ApplicationPort != 17101 {
		t.Errorf("ports = %d/%d, want 7101/17101", dash.SupervisorPort, dash.ApplicationPort)
	}
	if dash.Command != "npm run dev -- --port 17101 --strictPort" {
		t.Errorf("Command = %q", dash.Command)
	}
	if dash.StartupTimeoutSeconds != 90 || dash.IdleTimeoutMinutes != 30 {
		t.Errorf("timeouts = %d/%d, want 90/30", dash.StartupTimeoutSeconds, dash.IdleTimeoutMinutes)
	}
	if dash.ShutdownSignal != "SIGINT" || dash.ShutdownTimeoutSeconds != 5 {
		t.Errorf("shutdown = %s/%d, want SIGINT/5", dash.ShutdownSignal, dash.ShutdownTimeoutSeconds)
	}
	if *dash.WebSocketsKeepAlive {
		t.Error("WebSocketsKeepAlive = true, want false (explicitly set)")
	}
	if !dash.AlwaysOn || dash.LogRetentionDays != 3 || dash.NodePath != "/usr/local/bin/node" {
		t.Errorf("optional fields = %v/%d/%q", dash.AlwaysOn, dash.LogRetentionDays, dash.NodePath)
	}
	if dash.Env["APP_ENV"] != "local" {
		t.Errorf("Env = %v, want APP_ENV=local", dash.Env)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "valid.yaml"))
	if err != nil {
		t.Fatalf("Load(valid.yaml) error: %v", err)
	}

	p := cfg.Projects["minimal"]
	if p.ListenHost != DefaultListenHost {
		t.Errorf("ListenHost = %q, want %q", p.ListenHost, DefaultListenHost)
	}
	if p.ReadinessStrategy != ReadinessHTTP {
		t.Errorf("ReadinessStrategy = %q, want %q", p.ReadinessStrategy, ReadinessHTTP)
	}
	if want := "http://127.0.0.1:17102/"; p.ReadinessURL != want {
		t.Errorf("ReadinessURL = %q, want %q", p.ReadinessURL, want)
	}
	if p.StartupTimeoutSeconds != DefaultStartupTimeoutSeconds {
		t.Errorf("StartupTimeoutSeconds = %d, want %d", p.StartupTimeoutSeconds, DefaultStartupTimeoutSeconds)
	}
	if p.IdleTimeoutMinutes != DefaultIdleTimeoutMinutes {
		t.Errorf("IdleTimeoutMinutes = %d, want %d", p.IdleTimeoutMinutes, DefaultIdleTimeoutMinutes)
	}
	if p.ShutdownSignal != DefaultShutdownSignal {
		t.Errorf("ShutdownSignal = %q, want %q", p.ShutdownSignal, DefaultShutdownSignal)
	}
	if p.ShutdownTimeoutSeconds != DefaultShutdownTimeoutSeconds {
		t.Errorf("ShutdownTimeoutSeconds = %d, want %d", p.ShutdownTimeoutSeconds, DefaultShutdownTimeoutSeconds)
	}
	if p.WebSocketsKeepAlive == nil || !*p.WebSocketsKeepAlive {
		t.Errorf("WebSocketsKeepAlive = %v, want true", p.WebSocketsKeepAlive)
	}
	if p.LogRetentionDays != DefaultLogRetentionDays {
		t.Errorf("LogRetentionDays = %d, want %d", p.LogRetentionDays, DefaultLogRetentionDays)
	}
	if p.AlwaysOn {
		t.Error("AlwaysOn = true, want false by default")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load(missing) error = %v, want os.ErrNotExist", err)
	}
}

func TestLoadMalformedYAML(t *testing.T) {
	_, err := Load(filepath.Join("testdata", "malformed.yaml"))
	if err == nil {
		t.Fatal("Load(malformed.yaml) succeeded, want parse error")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error = %q, want a parse error", err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	_, err := Load(filepath.Join("testdata", "unknown_field.yaml"))
	if err == nil {
		t.Fatal("Load(unknown_field.yaml) succeeded, want error for idle_timeout_mins")
	}
	if !strings.Contains(err.Error(), "idle_timeout_mins") {
		t.Errorf("error = %q, want mention of the unknown field idle_timeout_mins", err)
	}
}

func TestLoadHomeRelativeWorkingDirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `projects:
  homey:
    public_url: https://homey.test
    supervisor_port: 7101
    application_port: 17101
    working_directory: "~"
    command: npm run dev
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if got := cfg.Projects["homey"].WorkingDirectory; got != home {
		t.Errorf("WorkingDirectory = %q, want home dir %q", got, home)
	}
}

// loadInvalid loads a fixture that must fail validation and returns the
// combined error text.
func loadInvalid(t *testing.T, fixture string) string {
	t.Helper()
	_, err := Load(filepath.Join("testdata", fixture))
	if err == nil {
		t.Fatalf("Load(%s) succeeded, want validation error", fixture)
	}
	return err.Error()
}

// wantError asserts that the combined validation output contains an error
// naming the given project and field.
func wantError(t *testing.T, got, project, field string) {
	t.Helper()
	needle := `project "` + project + `": ` + field + `:`
	if !strings.Contains(got, needle) {
		t.Errorf("validation errors missing %q; got:\n%s", needle, got)
	}
}

func TestValidateMissingRequiredFieldsReportsAll(t *testing.T) {
	got := loadInvalid(t, "missing_required.yaml")

	for _, field := range []string{
		"public_url",
		"supervisor_port",
		"application_port",
		"working_directory",
		"command",
	} {
		wantError(t, got, "hollow", field)
	}
}

func TestValidateDuplicateSupervisorPort(t *testing.T) {
	got := loadInvalid(t, "duplicate_supervisor_port.yaml")

	wantError(t, got, "beta", "supervisor_port")
	if !strings.Contains(got, `"alpha"`) {
		t.Errorf("error should name the other claimant alpha; got:\n%s", got)
	}
}

func TestValidateDuplicateApplicationPort(t *testing.T) {
	got := loadInvalid(t, "duplicate_application_port.yaml")

	wantError(t, got, "beta", "application_port")
	if !strings.Contains(got, `"alpha"`) {
		t.Errorf("error should name the other claimant alpha; got:\n%s", got)
	}
}

func TestValidateCrossPortConflict(t *testing.T) {
	got := loadInvalid(t, "cross_port_conflict.yaml")

	wantError(t, got, "beta", "supervisor_port")
	if !strings.Contains(got, "application_port") {
		t.Errorf("error should name alpha's application_port; got:\n%s", got)
	}
}

func TestValidateNonLoopbackListenHost(t *testing.T) {
	got := loadInvalid(t, "non_loopback.yaml")

	wantError(t, got, "exposed", "listen_host")
	if !strings.Contains(got, "allow_non_loopback") {
		t.Errorf("error should point at the allow_non_loopback opt-in; got:\n%s", got)
	}
}

func TestValidateNonLoopbackAllowedWhenOptedIn(t *testing.T) {
	if _, err := Load(filepath.Join("testdata", "non_loopback_allowed.yaml")); err != nil {
		t.Fatalf("Load(non_loopback_allowed.yaml) error: %v, want success", err)
	}
}

func TestValidateNonexistentWorkingDirectory(t *testing.T) {
	got := loadInvalid(t, "missing_working_directory.yaml")

	wantError(t, got, "ghost", "working_directory")
	if !strings.Contains(got, "/nonexistent/herd-wake/ghost") {
		t.Errorf("error should include the missing path; got:\n%s", got)
	}
}

func TestValidateMalformedURLs(t *testing.T) {
	got := loadInvalid(t, "bad_urls.yaml")

	wantError(t, got, "mangled", "public_url")
	wantError(t, got, "mangled", "readiness_url")
}

func TestValidateBadOptionalFields(t *testing.T) {
	got := loadInvalid(t, "bad_fields.yaml")

	wantError(t, got, "chaotic", "supervisor_port")
	wantError(t, got, "chaotic", "readiness_strategy")
	wantError(t, got, "chaotic", "shutdown_signal")
	wantError(t, got, "chaotic", "idle_timeout_minutes")
	wantError(t, got, "twinned", "application_port")
}

// TestSampleConfigMatchesSchema guards config.sample.yaml against drifting
// from the schema: it must decode strictly, and the only acceptable
// validation errors are the example working directories not existing on this
// machine.
func TestSampleConfigMatchesSchema(t *testing.T) {
	_, err := Load(filepath.Join("..", "..", "config.sample.yaml"))
	if err == nil {
		return
	}
	for _, line := range strings.Split(err.Error(), "\n") {
		if !strings.Contains(line, "working_directory") {
			t.Errorf("config.sample.yaml no longer matches the schema: %s", line)
		}
	}
}

func TestDefaultPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error: %v", err)
	}
	want := filepath.Join(home, "Library", "Application Support", "herd-wake", "config.yaml")
	if got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestBaseDirSharedWithDefaultPath(t *testing.T) {
	base, err := BaseDir()
	if err != nil {
		t.Fatalf("BaseDir() error: %v", err)
	}
	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error: %v", err)
	}
	if filepath.Dir(path) != base {
		t.Errorf("DefaultPath() = %q, want it inside BaseDir() %q", path, base)
	}
}
