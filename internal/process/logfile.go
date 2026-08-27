package process

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// maxLogFileBytes is the size at which a project's log file is rotated on
// the next project start.
const maxLogFileBytes = 10 << 20 // 10 MiB

// openLogFile prepares and opens the append-only log file for one project.
//
// log_retention_days is honored with a deliberately simple scheme, applied
// each time the project starts (no background sweeper):
//
//   - a log file larger than maxLogFileBytes is rotated to <name>.log.old
//     (replacing any previous rotation), so at most two files exist;
//   - the rotation is deleted once it has not been written for
//     retentionDays days;
//   - a current log that has not been written for retentionDays days is
//     deleted (along with its rotation) and the project starts fresh.
//
// Pruning is best-effort: an unremovable old file never blocks a start.
func openLogFile(path string, retentionDays int) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	pruneLogs(path, retentionDays)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return f, nil
}

// pruneLogs applies the rotation and retention policy described on
// openLogFile.
func pruneLogs(path string, retentionDays int) {
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	rotated := path + ".old"

	if info, err := os.Stat(rotated); err == nil && info.ModTime().Before(cutoff) {
		_ = os.Remove(rotated)
	}

	info, err := os.Stat(path)
	if err != nil {
		return
	}
	switch {
	case info.ModTime().Before(cutoff):
		// The whole log predates the retention window; any rotation is at
		// least as old.
		_ = os.Remove(path)
		_ = os.Remove(rotated)
	case info.Size() > maxLogFileBytes:
		_ = os.Rename(path, rotated)
	}
}
