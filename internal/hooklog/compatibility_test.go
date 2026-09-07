package hooklog

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOpenPreservesPreUpgradeLogs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	dir := filepath.Join(home, "logs", "hooks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := make(map[string]bool)
	for i := 0; i < 25; i++ {
		// #4012 used these filenames and passed unlocked descriptors to
		// hooks that can remain alive across the upgrade to this version.
		file, err := os.CreateTemp(dir, "post-worktree-*.log")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		old := time.Now().Add(-30 * 24 * time.Hour)
		if err := os.Chtimes(file.Name(), old, old); err != nil {
			t.Fatal(err)
		}
		want[filepath.Base(file.Name())] = true
	}
	opened, err := Open(OnArchive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	want[filepath.Base(opened.Name())] = true
	assertLogNames(t, dir, want)
}

func TestOpenLockUnavailableStillCapturesOutput(t *testing.T) {
	for _, lockErr := range []syscall.Errno{syscall.ENOSYS, syscall.EOPNOTSUPP} {
		t.Run(lockErr.Error(), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			dir := filepath.Join(home, "logs", "hooks")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			seedLog(t, dir, "post-worktree-v1-kept.log", time.Now().Add(-30*24*time.Hour))
			file, err := open(PostWorktree, func(int, int) error { return lockErr })
			if err != nil {
				t.Fatalf("unsupported flock prevented hook output capture: %v", err)
			}
			t.Cleanup(func() { _ = file.Close() })
			if strings.HasPrefix(filepath.Base(file.Name()), "post-worktree-v1-") {
				t.Fatal("unlocked fallback was advertised as lock-aware")
			}
			if _, err := file.WriteString("hook still runs\n"); err != nil {
				t.Fatal(err)
			}
			tail, err := CloseAndReadTail(file)
			if err != nil || tail != "hook still runs\n" {
				t.Fatalf("fallback output = %q, %v", tail, err)
			}
			assertLogNames(t, dir, map[string]bool{
				"post-worktree-v1-kept.log": true,
				filepath.Base(file.Name()):  true,
			})
		})
	}
}
