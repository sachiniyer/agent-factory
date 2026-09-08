package hooklog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCreatesPrivateLogUnderAFHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	file, err := Open(PostWorktree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })

	wantDir := filepath.Join(home, "logs", "hooks")
	if filepath.Dir(file.Name()) != wantDir {
		t.Fatalf("hook log directory = %s, want %s", filepath.Dir(file.Name()), wantDir)
	}
	if mode := fileMode(t, file.Name()); mode != 0o600 {
		t.Fatalf("hook log mode = %o, want 600", mode)
	}
	if mode := fileMode(t, wantDir); mode != 0o700 {
		t.Fatalf("hook log directory mode = %o, want 700", mode)
	}
}

func TestCloseAndReadTailKeepsOnlyBoundedEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	file, err := Open(OnArchive)
	if err != nil {
		t.Fatal(err)
	}

	want := strings.Repeat("z", TailLimit)
	if _, err := file.WriteString("discarded-prefix" + want); err != nil {
		t.Fatal(err)
	}
	tail, err := CloseAndReadTail(file)
	if err != nil {
		t.Fatal(err)
	}
	want = "[output truncated to last 65536 bytes]\n" + want
	if tail != want {
		t.Fatalf("bounded tail mismatch: got %d bytes, want %d", len(tail), len(want))
	}
}

func TestOpenRejectsUnknownKind(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	if _, err := Open(Kind("../../escape")); err == nil {
		t.Fatal("unknown hook log kind was accepted as a filename prefix")
	}
}

func TestCloseAndReadTailUsesOpenedFileNotReplacedPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	file, err := Open(PostWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("original hook output"); err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement path output"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	tail, err := CloseAndReadTail(file)
	if err != nil {
		t.Fatal(err)
	}
	if tail != "original hook output" {
		t.Fatalf("tail = %q, want bytes from the descriptor handed to the hook", tail)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("completion touched replacement path: mtime = %s, want %s", info.ModTime(), old)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
