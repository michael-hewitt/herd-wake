package process

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenLogFileCreatesDirectoryAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "demo.log")

	f, err := openLogFile(path, 7)
	if err != nil {
		t.Fatalf("openLogFile: %v", err)
	}
	if _, err := f.WriteString("one\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	f, err = openLogFile(path, 7)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := f.WriteString("two\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\n" {
		t.Errorf("log content = %q, want appended lines", data)
	}
}

func TestOpenLogFileRotatesOversizedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.log")
	if err := os.WriteFile(path, []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sparse file over the rotation threshold, without writing 10MB.
	if err := os.Truncate(path, maxLogFileBytes+1); err != nil {
		t.Fatal(err)
	}

	f, err := openLogFile(path, 7)
	if err != nil {
		t.Fatalf("openLogFile: %v", err)
	}
	defer f.Close() //nolint:errcheck // test cleanup

	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Errorf("current log should be fresh after rotation (size=%v, err=%v)", info, err)
	}
	if info, err := os.Stat(path + ".old"); err != nil || info.Size() != maxLogFileBytes+1 {
		t.Errorf("rotation should hold the old content (info=%v, err=%v)", info, err)
	}
}

func TestOpenLogFilePrunesExpiredLogs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.log")
	rotated := path + ".old"
	expired := time.Now().AddDate(0, 0, -10)
	for _, p := range []string{path, rotated} {
		if err := os.WriteFile(p, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, expired, expired); err != nil {
			t.Fatal(err)
		}
	}

	f, err := openLogFile(path, 7)
	if err != nil {
		t.Fatalf("openLogFile: %v", err)
	}
	defer f.Close() //nolint:errcheck // test cleanup

	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Errorf("expired log should have been replaced with a fresh one (info=%v, err=%v)", info, err)
	}
	if _, err := os.Stat(rotated); !os.IsNotExist(err) {
		t.Errorf("expired rotation should be deleted (err=%v)", err)
	}
}

func TestOpenLogFileKeepsRecentLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.log")
	if err := os.WriteFile(path, []byte("recent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := openLogFile(path, 7)
	if err != nil {
		t.Fatalf("openLogFile: %v", err)
	}
	defer f.Close() //nolint:errcheck // test cleanup

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "recent\n" {
		t.Errorf("a recent, small log must be kept; got %q", data)
	}
}
