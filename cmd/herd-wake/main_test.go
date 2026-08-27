package main

import (
	"bytes"
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
