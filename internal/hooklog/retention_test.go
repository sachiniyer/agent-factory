package hooklog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

func TestOpenPrunesKeptLogs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	inFlight, err := Open(PostWorktree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inFlight.Close() })
	dir := filepath.Dir(inFlight.Name())
	now := time.Now()
	want := map[string]bool{filepath.Base(inFlight.Name()): true}
	for _, kind := range []Kind{PostWorktree, OnArchive} {
		for i := 0; i < 25; i++ {
			name := fmt.Sprintf("%s-kept-%02d.log", kind, i)
			seedLog(t, dir, name, now.Add(-time.Duration(i+1)*time.Hour))
			if i < 20 {
				want[name] = true
			}
		}
		for i := 0; i < 2; i++ {
			seedLog(t, dir, fmt.Sprintf("%s-expired-%d.log", kind, i), now.Add(-15*24*time.Hour))
		}
		// Recent logs are neither pruned nor charged against the kept quota.
		for i := 0; i < 22; i++ {
			name := fmt.Sprintf("%s-new-%02d.log", kind, i)
			seedLog(t, dir, name, now.Add(time.Hour))
			want[name] = true
		}
	}
	for _, name := range []string{"unrelated.log", "post-worktree-note.txt"} {
		seedLog(t, dir, name, now.Add(-30*24*time.Hour))
		want[name] = true
	}
	if err := os.Mkdir(filepath.Join(dir, "post-worktree-directory.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	want["post-worktree-directory.log"] = true
	if err := os.Symlink(filepath.Join(dir, "unrelated.log"), filepath.Join(dir, "on-archive-link.log")); err != nil {
		t.Fatal(err)
	}
	want["on-archive-link.log"] = true

	var output logtest.Buffer
	previous := aflog.InfoLog.Writer()
	aflog.InfoLog.SetOutput(&output)
	t.Cleanup(func() { aflog.InfoLog.SetOutput(previous) })
	opened, err := Open(OnArchive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	want[filepath.Base(opened.Name())] = true
	assertLogNames(t, dir, want)
	if got := output.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "pruned 14") {
		t.Errorf("prune INFO = %q, want one line naming pruned 14", got)
	}
	if _, err := inFlight.WriteString("still running\n"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenBelowRetentionLimitUntouched(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	first, err := Open(PostWorktree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	dir := filepath.Dir(first.Name())
	want := map[string]bool{filepath.Base(first.Name()): true}
	for _, kind := range []Kind{PostWorktree, OnArchive} {
		for i := 0; i < 19; i++ {
			name := fmt.Sprintf("%s-%d.log", kind, i)
			seedLog(t, dir, name, time.Now().Add(-24*time.Hour))
			want[name] = true
		}
	}
	opened, err := Open(OnArchive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	want[filepath.Base(opened.Name())] = true
	assertLogNames(t, dir, want)
}

func seedLog(t *testing.T, dir, name string, modified time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("hook output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func assertLogNames(t *testing.T, dir string, want map[string]bool) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool)
	for _, entry := range entries {
		got[entry.Name()] = true
		if !want[entry.Name()] {
			t.Errorf("unexpected retained log %s", entry.Name())
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("removed protected/kept log %s", name)
		}
	}
}
