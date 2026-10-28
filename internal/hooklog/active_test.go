package hooklog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenPreservesQuietActiveLogs(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	want := make(map[string]bool)
	var dir string
	for i := 0; i < 25; i++ {
		file, err := Open(PostWorktree)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		dir = filepath.Dir(file.Name())
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(file.Name(), old, old); err != nil {
			t.Fatal(err)
		}
		want[filepath.Base(file.Name())] = true
	}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("post-worktree-v1-kept-%02d.log", i)
		seedLog(t, dir, name, time.Now().Add(-24*time.Hour))
		want[name] = true
	}
	next, err := Open(PostWorktree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	want[filepath.Base(next.Name())] = true
	assertLogNames(t, dir, want)
}

func TestOpenPreservesInheritedLog(t *testing.T) {
	if os.Getenv("AF_TEST_HOOKLOG_INHERITED") == "1" {
		_, _ = io.Copy(os.Stdout, os.Stdin)
		os.Exit(23)
	}
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	file, err := Open(PostWorktree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	child := exec.Command(os.Args[0], "-test.run=^TestOpenPreservesInheritedLog$")
	child.Env = append(os.Environ(), "AF_TEST_HOOKLOG_INHERITED=1")
	child.Stdout, child.Stderr = file, file
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	// The launcher has disappeared; only the hook's inherited output
	// descriptor can keep this quiet log protected from another launcher.
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(file.Name(), old, old); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(OnArchive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	if _, err := os.Stat(file.Name()); err != nil {
		t.Errorf("quiet inherited hook log disappeared: %v", err)
	}
	if _, err := io.WriteString(input, "output after launcher exit\n"); err != nil {
		t.Fatal(err)
	}
	_ = input.Close()
	var exitErr *exec.ExitError
	if err := child.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("hook exit = %v, want 23", err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || string(data) != "output after launcher exit\n" {
		t.Fatalf("retained output = %q, %v", data, err)
	}
	if err := os.Chtimes(file.Name(), old, old); err != nil {
		t.Fatal(err)
	}
	next, err := Open(OnArchive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	if _, err := os.Stat(file.Name()); !os.IsNotExist(err) {
		t.Fatalf("finished expired hook log was not pruned: %v", err)
	}
}
