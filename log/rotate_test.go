package log

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The rotation tests (#1059) are hermetic: each one points AGENT_FACTORY_HOME
// at a fresh temp dir, so both the log file (resolveLogPath) and the rotation
// policy (rotationPolicy reads $AGENT_FACTORY_HOME/config.json) live entirely
// in the sandbox and never touch the developer's real log or config. A 1 MB
// cap keeps the fixtures small while still exercising the real MB-based
// config keys.

// setupRotationHome creates a sandboxed AGENT_FACTORY_HOME with the given
// rotation config and returns the log path inside it.
func setupRotationHome(t *testing.T, configJSON string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	if configJSON != "" {
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(configJSON), 0644); err != nil {
			t.Fatalf("write config.json: %v", err)
		}
	}
	return filepath.Join(home, "agent-factory.log")
}

// fillFile writes size bytes of marker-prefixed content to path.
func fillFile(t *testing.T, path, marker string, size int) {
	t.Helper()
	content := append([]byte(marker+"\n"), bytes.Repeat([]byte("x"), size)...)
	if err := os.WriteFile(path, content[:size], 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestInitializeRotatesOverCap(t *testing.T) {
	logPath := setupRotationHome(t, `{"log_max_size_mb": 1, "log_max_backups": 2}`)
	fillFile(t, logPath, "OLD-LOG-CONTENT", 1<<20+512) // just over the 1 MB cap

	Initialize(false)
	defer Close()

	InfoLog.Printf("fresh-after-rotation")

	rotated, err := os.ReadFile(logPath + ".1")
	if err != nil {
		t.Fatalf("expected rotated file %s.1 to exist: %v", logPath, err)
	}
	if !strings.HasPrefix(string(rotated), "OLD-LOG-CONTENT") {
		t.Fatalf("rotated file does not carry the old log content; starts with %q", string(rotated[:32]))
	}

	current, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected fresh log file: %v", err)
	}
	if strings.Contains(string(current), "OLD-LOG-CONTENT") {
		t.Fatalf("fresh log still contains pre-rotation content")
	}
	if !strings.Contains(string(current), "fresh-after-rotation") {
		t.Fatalf("fresh log missing post-rotation write; got %q", string(current))
	}
	if len(current) >= 1<<20 {
		t.Fatalf("fresh log is not fresh: %d bytes", len(current))
	}
}

func TestInitializeNoRotateUnderCap(t *testing.T) {
	logPath := setupRotationHome(t, `{"log_max_size_mb": 1, "log_max_backups": 2}`)
	fillFile(t, logPath, "UNDER-CAP-CONTENT", 100<<10) // 100 KB, well under 1 MB

	Initialize(false)
	defer Close()

	InfoLog.Printf("appended-after-init")

	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Fatalf("expected no rotation under the cap; stat .1 err = %v", err)
	}
	current, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.HasPrefix(string(current), "UNDER-CAP-CONTENT") {
		t.Fatalf("under-cap log was not preserved")
	}
	if !strings.Contains(string(current), "appended-after-init") {
		t.Fatalf("expected append to the existing under-cap log")
	}
}

func TestRotationRetentionPrunesBeyondKeepCount(t *testing.T) {
	logPath := setupRotationHome(t, `{"log_max_size_mb": 1, "log_max_backups": 2}`)
	fillFile(t, logPath, "CURRENT", 1<<20+512)
	fillFile(t, logPath+".1", "BACKUP-1", 64)
	fillFile(t, logPath+".2", "BACKUP-2", 64)
	// A .3 left over from an earlier, larger log_max_backups must be pruned.
	fillFile(t, logPath+".3", "BACKUP-3", 64)

	Initialize(false)
	defer Close()

	one, err := os.ReadFile(logPath + ".1")
	if err != nil {
		t.Fatalf("read .1: %v", err)
	}
	if !strings.HasPrefix(string(one), "CURRENT") {
		t.Fatalf(".1 should hold the pre-rotation current log; starts with %q", string(one[:16]))
	}
	two, err := os.ReadFile(logPath + ".2")
	if err != nil {
		t.Fatalf("read .2: %v", err)
	}
	if !strings.HasPrefix(string(two), "BACKUP-1") {
		t.Fatalf(".2 should hold the old .1; got %q", string(two))
	}
	if _, err := os.Stat(logPath + ".3"); !os.IsNotExist(err) {
		t.Fatalf("expected .3 to be pruned (keep count 2); stat err = %v", err)
	}
}

func TestZeroBackupsDeletesOnRotation(t *testing.T) {
	logPath := setupRotationHome(t, `{"log_max_size_mb": 1, "log_max_backups": 0}`)
	fillFile(t, logPath, "DOOMED", 1<<20+512)

	Initialize(false)
	defer Close()

	InfoLog.Printf("post-truncate-write")

	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Fatalf("log_max_backups=0 must keep no rotated files; stat .1 err = %v", err)
	}
	current, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(current), "DOOMED") {
		t.Fatalf("old content survived a zero-backup rotation")
	}
	if len(current) >= 1<<20 {
		t.Fatalf("log not truncated: %d bytes", len(current))
	}
}

// TestWritePathRotation covers the long-running-daemon case: a process that
// never re-runs Initialize must still rotate once its own writes push the
// file past the cap, and no message may be lost across the rotation.
func TestWritePathRotation(t *testing.T) {
	logPath := setupRotationHome(t, `{"log_max_size_mb": 1, "log_max_backups": 1}`)

	Initialize(false)
	defer Close()

	// Each line is ~1 KB; 1200 of them cross the 1 MB cap mid-run.
	payload := strings.Repeat("p", 1024)
	const writes = 1200
	for i := 0; i < writes; i++ {
		InfoLog.Printf("WPMARK %d %s", i, payload)
	}

	rotated, err := os.ReadFile(logPath + ".1")
	if err != nil {
		t.Fatalf("expected write-path rotation to produce %s.1: %v", logPath, err)
	}
	current, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read current log: %v", err)
	}
	if len(current) > 1<<20 {
		t.Fatalf("current log exceeds the cap after write-path rotation: %d bytes", len(current))
	}

	combined := string(rotated) + string(current)
	for i := 0; i < writes; i++ {
		if !strings.Contains(combined, fmt.Sprintf("WPMARK %d ", i)) {
			t.Fatalf("message %d lost across write-path rotation", i)
		}
	}
}

// TestRotationPolicyDefaults pins the policy resolution: defaults when no
// config exists, overrides applied when present, invalid values ignored.
func TestRotationPolicyDefaults(t *testing.T) {
	t.Run("no config file -> defaults", func(t *testing.T) {
		setupRotationHome(t, "")
		maxBytes, backups := rotationPolicy()
		if maxBytes != int64(DefaultMaxSizeMB)<<20 || backups != DefaultMaxBackups {
			t.Fatalf("got maxBytes=%d backups=%d, want %d MB / %d", maxBytes, backups, DefaultMaxSizeMB, DefaultMaxBackups)
		}
	})
	t.Run("config overrides", func(t *testing.T) {
		setupRotationHome(t, `{"log_max_size_mb": 10, "log_max_backups": 5}`)
		maxBytes, backups := rotationPolicy()
		if maxBytes != 10<<20 || backups != 5 {
			t.Fatalf("got maxBytes=%d backups=%d, want 10 MB / 5", maxBytes, backups)
		}
	})
	t.Run("invalid values -> defaults", func(t *testing.T) {
		setupRotationHome(t, `{"log_max_size_mb": -3, "log_max_backups": -1}`)
		maxBytes, backups := rotationPolicy()
		if maxBytes != int64(DefaultMaxSizeMB)<<20 || backups != DefaultMaxBackups {
			t.Fatalf("got maxBytes=%d backups=%d, want defaults", maxBytes, backups)
		}
	})
	t.Run("corrupt config -> defaults", func(t *testing.T) {
		setupRotationHome(t, `{not json`)
		maxBytes, backups := rotationPolicy()
		if maxBytes != int64(DefaultMaxSizeMB)<<20 || backups != DefaultMaxBackups {
			t.Fatalf("got maxBytes=%d backups=%d, want defaults", maxBytes, backups)
		}
	})
}
