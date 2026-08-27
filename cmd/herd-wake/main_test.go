package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"version"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(version) exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.HasPrefix(out, "herd-wake ") {
		t.Errorf("run(version) stdout = %q, want prefix %q", out, "herd-wake ")
	}
	if strings.TrimSpace(strings.TrimPrefix(out, "herd-wake")) == "" {
		t.Errorf("run(version) stdout = %q, want a version string after the binary name", out)
	}
}

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run(nil, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run() exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("run() stderr = %q, want usage text", stderr.String())
	}
}

func TestRunProjectsListsConfiguredProjects(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"projects", "--config", "testdata/config.yaml"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(projects) exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"dashboard",
		"https://dashboard.test",
		"7101",
		"17101",
		"npm run dev -- --port 17101 --strictPort",
		"accounts-vite",
		"https://vite.accounts.test",
		"7102",
		"17102",
		"(always on)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run(projects) stdout missing %q; got:\n%s", want, out)
		}
	}
}

func TestRunProjectsMissingConfigFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "config.yaml")

	code := run([]string{"projects", "--config", missing}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run(projects, missing config) exit code = %d, want 1", code)
	}
	msg := stderr.String()
	if !strings.Contains(msg, missing) {
		t.Errorf("stderr should name the missing path %q; got:\n%s", missing, msg)
	}
	for _, want := range []string{"projects:", "public_url:", "supervisor_port:", "command:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr should include a sample config with %q; got:\n%s", want, msg)
		}
	}
}

func TestRunProjectsInvalidConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"projects", "--config", "testdata/invalid.yaml"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run(projects, invalid config) exit code = %d, want 1", code)
	}
	msg := stderr.String()
	if !strings.Contains(msg, `project "broken"`) {
		t.Errorf("stderr should name the offending project; got:\n%s", msg)
	}
	if !strings.Contains(msg, "command") || !strings.Contains(msg, "public_url") {
		t.Errorf("stderr should name the offending fields; got:\n%s", msg)
	}
}

func TestRunProjectsBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"projects", "--bogus"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(projects --bogus) exit code = %d, want 2", code)
	}
}

func TestUsageMentionsProjects(t *testing.T) {
	var stdout, stderr bytes.Buffer

	run(nil, &stdout, &stderr)

	if !strings.Contains(stderr.String(), "projects") {
		t.Errorf("usage should mention the projects command; got:\n%s", stderr.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"bogus"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(bogus) exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "bogus"`) {
		t.Errorf("run(bogus) stderr = %q, want unknown-command diagnostic", stderr.String())
	}
}
