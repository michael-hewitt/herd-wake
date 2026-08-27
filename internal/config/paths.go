package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// BaseDir returns the herd-wake base directory,
// ~/Library/Application Support/herd-wake. All user-level herd-wake state
// (config, and in later slices the control socket and logs) lives under it.
func BaseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "herd-wake"), nil
}

// DefaultPath returns the default config file path,
// ~/Library/Application Support/herd-wake/config.yaml.
func DefaultPath() (string, error) {
	base, err := BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "config.yaml"), nil
}

// LogsDir returns the default directory for per-project process logs,
// ~/Library/Application Support/herd-wake/logs. Each project's combined
// stdout/stderr is appended to <name>.log inside it.
func LogsDir() (string, error) {
	base, err := BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "logs"), nil
}

// SocketPath returns the default control socket path,
// ~/Library/Application Support/herd-wake/herd-wake.sock. The daemon listens
// on it and the CLI connects to it.
func SocketPath() (string, error) {
	base, err := BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "herd-wake.sock"), nil
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
