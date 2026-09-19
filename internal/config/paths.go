package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Paths is the set of default locations herd-wake uses for its user-level
// state. They depend on the platform (see DefaultPaths).
type Paths struct {
	// BaseDir holds the config file; projects.d and sync-state.yaml resolve
	// relative to the config file, so they live here too.
	BaseDir string
	// ConfigFile is the default config file, BaseDir/config.yaml.
	ConfigFile string
	// LogsDir is the default directory for per-project process logs.
	LogsDir string
	// SocketPath is the default control socket.
	SocketPath string
}

// DefaultPaths returns the default paths for the running platform:
//
//   - macOS: everything under ~/Library/Application Support/herd-wake —
//     config.yaml, logs/, and herd-wake.sock.
//   - Linux (and any other OS): XDG conventions. Config in
//     $XDG_CONFIG_HOME/herd-wake (fallback ~/.config/herd-wake), logs in
//     $XDG_STATE_HOME/herd-wake/logs (fallback ~/.local/state/herd-wake/logs),
//     socket $XDG_RUNTIME_DIR/herd-wake.sock, falling back to
//     /run/user/<uid>/herd-wake.sock when that directory exists (so a shell
//     without XDG_RUNTIME_DIR — cron, su, docker exec — still finds the
//     socket of a daemon running under the systemd user manager) and
//     otherwise to /tmp/herd-wake-<uid>/herd-wake.sock. An XDG variable is
//     honoured only when it holds an absolute path, as the XDG base
//     directory spec requires.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home directory: %w", err)
	}
	return defaultPaths(runtime.GOOS, os.Getenv, home, os.Getuid(), dirExists), nil
}

// defaultPaths is DefaultPaths with the platform inputs injected: goos is the
// runtime.GOOS value, getenv looks up environment variables, home is the
// user's home directory, uid the numeric user id (used for the per-user
// socket fallbacks on Linux) and dirExists reports whether a directory
// exists (used to detect /run/user/<uid>).
func defaultPaths(goos string, getenv func(string) string, home string, uid int, dirExists func(string) bool) Paths {
	if goos == "darwin" {
		base := filepath.Join(home, "Library", "Application Support", "herd-wake")
		return Paths{
			BaseDir:    base,
			ConfigFile: filepath.Join(base, "config.yaml"),
			LogsDir:    filepath.Join(base, "logs"),
			SocketPath: filepath.Join(base, "herd-wake.sock"),
		}
	}

	configHome := xdgDir(getenv, "XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	stateHome := xdgDir(getenv, "XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	runtimeDir := xdgDir(getenv, "XDG_RUNTIME_DIR", runtimeDirFallback(uid, dirExists))

	base := filepath.Join(configHome, "herd-wake")
	return Paths{
		BaseDir:    base,
		ConfigFile: filepath.Join(base, "config.yaml"),
		LogsDir:    filepath.Join(stateHome, "herd-wake", "logs"),
		SocketPath: filepath.Join(runtimeDir, "herd-wake.sock"),
	}
}

// runtimeDirFallback is the socket directory when XDG_RUNTIME_DIR is unset:
// the systemd user manager's /run/user/<uid> when it exists (the daemon under
// contrib/herd-wake.service listens there, and the CLI must find it even from
// a shell that does not carry the variable), otherwise a private /tmp
// directory.
func runtimeDirFallback(uid int, dirExists func(string) bool) string {
	if runDir := filepath.Join("/run", "user", strconv.Itoa(uid)); dirExists(runDir) {
		return runDir
	}
	return filepath.Join("/tmp", fmt.Sprintf("herd-wake-%d", uid))
}

// dirExists reports whether path is an existing directory (following
// symlinks).
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// xdgDir returns the value of the XDG base directory variable name when it
// is set to an absolute path, otherwise fallback. The XDG spec says a
// relative value must be ignored.
func xdgDir(getenv func(string) string, name, fallback string) string {
	if v := getenv(name); filepath.IsAbs(v) {
		return filepath.Clean(v)
	}
	return fallback
}

// BaseDir returns the directory the default config file lives in:
// ~/Library/Application Support/herd-wake on macOS, $XDG_CONFIG_HOME/herd-wake
// (fallback ~/.config/herd-wake) on Linux.
func BaseDir() (string, error) {
	p, err := DefaultPaths()
	return p.BaseDir, err
}

// DefaultPath returns the default config file path:
// ~/Library/Application Support/herd-wake/config.yaml on macOS,
// $XDG_CONFIG_HOME/herd-wake/config.yaml (fallback
// ~/.config/herd-wake/config.yaml) on Linux.
func DefaultPath() (string, error) {
	p, err := DefaultPaths()
	return p.ConfigFile, err
}

// LogsDir returns the default directory for per-project process logs:
// ~/Library/Application Support/herd-wake/logs on macOS,
// $XDG_STATE_HOME/herd-wake/logs (fallback ~/.local/state/herd-wake/logs) on
// Linux. Each project's combined stdout/stderr is appended to <name>.log
// inside it.
func LogsDir() (string, error) {
	p, err := DefaultPaths()
	return p.LogsDir, err
}

// SocketPath returns the default control socket path:
// ~/Library/Application Support/herd-wake/herd-wake.sock on macOS,
// $XDG_RUNTIME_DIR/herd-wake.sock (fallback /run/user/<uid>/herd-wake.sock
// when that directory exists, else /tmp/herd-wake-<uid>/herd-wake.sock) on
// Linux. The daemon listens on it and the CLI connects to it.
func SocketPath() (string, error) {
	p, err := DefaultPaths()
	return p.SocketPath, err
}

// DisplayDefaults returns the default paths in a form suitable for help
// text: the home directory prefix is abbreviated to "~". If the home
// directory cannot be resolved it falls back to the platform's home-relative
// defaults spelled with "~" literally, so help output never fails.
func DisplayDefaults() Paths {
	home, err := os.UserHomeDir()
	if err != nil {
		return defaultPaths(runtime.GOOS, os.Getenv, "~", os.Getuid(), dirExists)
	}
	p := defaultPaths(runtime.GOOS, os.Getenv, home, os.Getuid(), dirExists)
	return Paths{
		BaseDir:    abbreviateHome(p.BaseDir, home),
		ConfigFile: abbreviateHome(p.ConfigFile, home),
		LogsDir:    abbreviateHome(p.LogsDir, home),
		SocketPath: abbreviateHome(p.SocketPath, home),
	}
}

// abbreviateHome replaces a leading home directory in path with "~". Paths
// outside home, and an empty home, are returned unchanged.
func abbreviateHome(path, home string) string {
	home = filepath.Clean(home)
	if home == "" || home == "." || home == string(filepath.Separator) {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

// expandHome replaces a leading "~" or "~/" in path with the current user's
// home directory. Any other path is returned unchanged.
func expandHome(path string) string {
	if path != "~" && !hasHomePrefix(path) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

func hasHomePrefix(path string) bool {
	return len(path) >= 2 && path[0] == '~' && path[1] == '/'
}
