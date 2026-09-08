package hooklog

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCloseAndReadTailProtectsJustCompletedQuietLog(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	file, err := Open(PostWorktree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.WriteString("quiet failure output\n"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(file.Name())
	want := map[string]bool{filepath.Base(file.Name()): true}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("post-worktree-v1-kept-%02d.log", i)
		seedLog(t, dir, name, time.Now().Add(-time.Duration(i+1)*time.Hour))
		want[name] = true
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(file.Name(), old, old); err != nil {
		t.Fatal(err)
	}
	completed := time.Now()
	tail, err := CloseAndReadTail(file)
	if err != nil || tail != "quiet failure output\n" {
		t.Fatalf("completed output = %q, %v", tail, err)
	}
	info, err := os.Stat(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Before(completed.Truncate(time.Second)) {
		t.Errorf("completion left stale retention timestamp %s", info.ModTime())
	}
	next, err := Open(PostWorktree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	want[filepath.Base(next.Name())] = true
	assertLogNames(t, dir, want)

	// After the grace window, completion time still makes this the newest
	// kept log: the oldest of the other 20 is the one that must be removed.
	count, err := prune(dir, next.Name(), time.Now().Add(logGraceAge+time.Second))
	if err != nil || count != 1 {
		t.Fatalf("prune after grace = %d, %v; want 1, nil", count, err)
	}
	delete(want, "post-worktree-v1-kept-19.log")
	assertLogNames(t, dir, want)
}
