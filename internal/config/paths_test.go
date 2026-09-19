package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// envOf returns a getenv function backed by the given map; unset names
// resolve to "".
func envOf(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// noDirs is a dirExists that finds nothing; runUserDir finds only
// /run/user/1000.
func noDirs(string) bool { return false }

func runUserDir(path string) bool { return path == "/run/user/1000" }

func TestDefaultPathsPerPlatform(t *testing.T) {
	const home = "/home/alice"
	const uid = 1000

	tests := []struct {
		name      string
		goos      string
		env       map[string]string
		dirExists func(string) bool
		want      Paths
	}{
		{
			name: "darwin ignores XDG and uses Application Support",
			goos: "darwin",
			env: map[string]string{
				"XDG_CONFIG_HOME": "/xdg/config",
				"XDG_STATE_HOME":  "/xdg/state",
				"XDG_RUNTIME_DIR": "/xdg/run",
			},
			want: Paths{
				BaseDir:    "/home/alice/Library/Application Support/herd-wake",
				ConfigFile: "/home/alice/Library/Application Support/herd-wake/config.yaml",
				LogsDir:    "/home/alice/Library/Application Support/herd-wake/logs",
				SocketPath: "/home/alice/Library/Application Support/herd-wake/herd-wake.sock",
			},
		},
		{
			name: "linux with all XDG variables set",
			goos: "linux",
			env: map[string]string{
				"XDG_CONFIG_HOME": "/xdg/config",
				"XDG_STATE_HOME":  "/xdg/state",
				"XDG_RUNTIME_DIR": "/run/user/1000",
			},
			want: Paths{
				BaseDir:    "/xdg/config/herd-wake",
				ConfigFile: "/xdg/config/herd-wake/config.yaml",
				LogsDir:    "/xdg/state/herd-wake/logs",
				SocketPath: "/run/user/1000/herd-wake.sock",
			},
		},
		{
			name: "linux with no XDG variables falls back to home and /tmp",
			goos: "linux",
			env:  map[string]string{},
			want: Paths{
				BaseDir:    "/home/alice/.config/herd-wake",
				ConfigFile: "/home/alice/.config/herd-wake/config.yaml",
				LogsDir:    "/home/alice/.local/state/herd-wake/logs",
				SocketPath: "/tmp/herd-wake-1000/herd-wake.sock",
			},
		},
		{
			name:      "linux without XDG_RUNTIME_DIR prefers an existing /run/user/<uid>",
			goos:      "linux",
			env:       map[string]string{},
			dirExists: runUserDir,
			want: Paths{
				BaseDir:    "/home/alice/.config/herd-wake",
				ConfigFile: "/home/alice/.config/herd-wake/config.yaml",
				LogsDir:    "/home/alice/.local/state/herd-wake/logs",
				SocketPath: "/run/user/1000/herd-wake.sock",
			},
		},
		{
			name:      "linux XDG_RUNTIME_DIR wins over an existing /run/user/<uid>",
			goos:      "linux",
			env:       map[string]string{"XDG_RUNTIME_DIR": "/xdg/run"},
			dirExists: runUserDir,
			want: Paths{
				BaseDir:    "/home/alice/.config/herd-wake",
				ConfigFile: "/home/alice/.config/herd-wake/config.yaml",
				LogsDir:    "/home/alice/.local/state/herd-wake/logs",
				SocketPath: "/xdg/run/herd-wake.sock",
			},
		},
		{
			name:      "darwin ignores an existing /run/user/<uid>",
			goos:      "darwin",
			env:       map[string]string{},
			dirExists: runUserDir,
			want: Paths{
				BaseDir:    "/home/alice/Library/Application Support/herd-wake",
				ConfigFile: "/home/alice/Library/Application Support/herd-wake/config.yaml",
				LogsDir:    "/home/alice/Library/Application Support/herd-wake/logs",
				SocketPath: "/home/alice/Library/Application Support/herd-wake/herd-wake.sock",
			},
		},
		{
			name: "linux ignores relative XDG values",
			goos: "linux",
			env: map[string]string{
				"XDG_CONFIG_HOME": "relative/config",
				"XDG_STATE_HOME":  "./state",
				"XDG_RUNTIME_DIR": "run",
			},
			want: Paths{
				BaseDir:    "/home/alice/.config/herd-wake",
				ConfigFile: "/home/alice/.config/herd-wake/config.yaml",
				LogsDir:    "/home/alice/.local/state/herd-wake/logs",
				SocketPath: "/tmp/herd-wake-1000/herd-wake.sock",
			},
		},
		{
			name: "linux cleans trailing slashes in XDG values",
			goos: "linux",
			env: map[string]string{
				"XDG_CONFIG_HOME": "/xdg/config/",
				"XDG_RUNTIME_DIR": "/run/user/1000/",
			},
			want: Paths{
				BaseDir:    "/xdg/config/herd-wake",
				ConfigFile: "/xdg/config/herd-wake/config.yaml",
				LogsDir:    "/home/alice/.local/state/herd-wake/logs",
				SocketPath: "/run/user/1000/herd-wake.sock",
			},
		},
		{
			name: "any other OS uses the XDG branch",
			goos: "freebsd",
			env:  map[string]string{"XDG_RUNTIME_DIR": "/var/run/user/1000"},
			want: Paths{
				BaseDir:    "/home/alice/.config/herd-wake",
				ConfigFile: "/home/alice/.config/herd-wake/config.yaml",
				LogsDir:    "/home/alice/.local/state/herd-wake/logs",
				SocketPath: "/var/run/user/1000/herd-wake.sock",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exists := tt.dirExists
			if exists == nil {
				exists = noDirs
			}
			got := defaultPaths(tt.goos, envOf(tt.env), home, uid, exists)
			if got != tt.want {
				t.Errorf("defaultPaths(%s) =\n  %+v\nwant\n  %+v", tt.goos, got, tt.want)
			}
		})
	}
}

func TestDefaultPathsSocketFallbackIsPerUser(t *testing.T) {
	a := defaultPaths("linux", envOf(nil), "/home/a", 1000, noDirs)
	b := defaultPaths("linux", envOf(nil), "/home/b", 1001, noDirs)
	if a.SocketPath == b.SocketPath {
		t.Errorf("socket fallback %q should differ per uid", a.SocketPath)
	}
	if a.SocketPath != "/tmp/herd-wake-1000/herd-wake.sock" {
		t.Errorf("SocketPath = %q, want /tmp/herd-wake-1000/herd-wake.sock", a.SocketPath)
	}
}

func TestRuntimeDirFallbackIsPerUser(t *testing.T) {
	exists := func(path string) bool { return path == "/run/user/1000" || path == "/run/user/1001" }
	if got := runtimeDirFallback(1000, exists); got != "/run/user/1000" {
		t.Errorf("runtimeDirFallback(1000) = %q, want /run/user/1000", got)
	}
	if got := runtimeDirFallback(1001, exists); got != "/run/user/1001" {
		t.Errorf("runtimeDirFallback(1001) = %q, want /run/user/1001", got)
	}
	if got := runtimeDirFallback(1002, exists); got != "/tmp/herd-wake-1002" {
		t.Errorf("runtimeDirFallback(1002) = %q, want /tmp/herd-wake-1002", got)
	}
}

func TestDirExists(t *testing.T) {
	dir := t.TempDir()
	if !dirExists(dir) {
		t.Errorf("dirExists(%q) = false for an existing directory", dir)
	}
	if dirExists(filepath.Join(dir, "missing")) {
		t.Error("dirExists reports a missing directory as present")
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if dirExists(file) {
		t.Error("dirExists reports a regular file as a directory")
	}
}

func TestExportedAccessorsMatchDefaultPaths(t *testing.T) {
	want, err := DefaultPaths()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	for _, tt := range []struct {
		name string
		fn   func() (string, error)
		want string
	}{
		{"BaseDir", BaseDir, want.BaseDir},
		{"DefaultPath", DefaultPath, want.ConfigFile},
		{"LogsDir", LogsDir, want.LogsDir},
		{"SocketPath", SocketPath, want.SocketPath},
	} {
		got, err := tt.fn()
		if err != nil {
			t.Errorf("%s() error: %v", tt.name, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestAbbreviateHome(t *testing.T) {
	tests := []struct {
		path, home, want string
	}{
		{"/home/alice/.config/herd-wake/config.yaml", "/home/alice", "~/.config/herd-wake/config.yaml"},
		{"/Users/bob/Library/Application Support/herd-wake", "/Users/bob", "~/Library/Application Support/herd-wake"},
		{"/home/alice", "/home/alice", "~"},
		{"/home/alice/", "/home/alice", "~/"},
		{"/home/alicent/x", "/home/alice", "/home/alicent/x"},
		{"/tmp/herd-wake-1000/herd-wake.sock", "/home/alice", "/tmp/herd-wake-1000/herd-wake.sock"},
		{"/run/user/1000/herd-wake.sock", "/home/alice/", "/run/user/1000/herd-wake.sock"},
		{"/home/alice/x", "/home/alice/", "~/x"},
		{"/home/alice/x", "", "/home/alice/x"},
		{"/home/alice/x", "/", "/home/alice/x"},
	}
	for _, tt := range tests {
		if got := abbreviateHome(tt.path, tt.home); got != tt.want {
			t.Errorf("abbreviateHome(%q, %q) = %q, want %q", tt.path, tt.home, got, tt.want)
		}
	}
}

func TestDisplayDefaultsAbbreviatesHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")

	got := DisplayDefaults()

	var wantConfig, wantLogs string
	if runtime.GOOS == "darwin" {
		wantConfig = "~/Library/Application Support/herd-wake/config.yaml"
		wantLogs = "~/Library/Application Support/herd-wake/logs"
	} else {
		wantConfig = "~/.config/herd-wake/config.yaml"
		wantLogs = "~/.local/state/herd-wake/logs"
	}
	if got.ConfigFile != wantConfig {
		t.Errorf("DisplayDefaults().ConfigFile = %q, want %q", got.ConfigFile, wantConfig)
	}
	if got.LogsDir != wantLogs {
		t.Errorf("DisplayDefaults().LogsDir = %q, want %q", got.LogsDir, wantLogs)
	}
	if got.SocketPath == "" {
		t.Error("DisplayDefaults().SocketPath is empty")
	}
}
