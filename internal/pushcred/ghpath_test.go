package pushcred

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExecutable(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, GHProgramName)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The property the Captain's approval pins: a relative or empty PATH entry can
// never make LookGH return a gh the working directory chose, even when that entry
// comes FIRST and the working directory holds a gh.
func TestLookGHNeverSearchesRelativeOrEmptyPathEntries(t *testing.T) {
	work := t.TempDir()
	writeExecutable(t, filepath.Join(work, "bin"))
	writeExecutable(t, work) // ./gh, reachable through an empty entry or "."
	real := writeExecutable(t, filepath.Join(t.TempDir(), "usr", "bin"))
	t.Chdir(work)

	list := strings.Join([]string{"", ".", "bin", "./bin", filepath.Dir(real)}, string(os.PathListSeparator))
	got, err := LookGH(list)
	if err != nil {
		t.Fatalf("LookGH: %v", err)
	}
	if got != real {
		t.Fatalf("LookGH resolved %q; want the absolute entry's %q", got, real)
	}

	// With only relative entries there is no answer at all, never the cwd's gh.
	if got, err := LookGH(strings.Join([]string{"", ".", "bin"}, string(os.PathListSeparator))); !errors.Is(err, ErrGHNotFound) {
		t.Fatalf("LookGH over relative entries = %q, %v; want ErrGHNotFound", got, err)
	}
}

func TestLookGHSkipsNamedDirectoriesAndNonExecutables(t *testing.T) {
	shim := writeExecutable(t, filepath.Join(t.TempDir(), "session"))
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plain, GHProgramName), []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirAsGH := filepath.Join(t.TempDir(), "dir")
	if err := os.MkdirAll(filepath.Join(dirAsGH, GHProgramName), 0o755); err != nil {
		t.Fatal(err)
	}
	real := writeExecutable(t, filepath.Join(t.TempDir(), "real"))

	list := strings.Join([]string{filepath.Dir(shim), plain, dirAsGH, filepath.Dir(real)}, string(os.PathListSeparator))
	got, err := LookGH(list, filepath.Dir(shim)+"/")
	if err != nil {
		t.Fatalf("LookGH: %v", err)
	}
	if got != real {
		t.Fatalf("LookGH = %q; want %q (the skipped dir, a non-executable file and a directory named gh are passed over)", got, real)
	}
	// Anti-vacuous: without the skip the shim's own directory wins.
	if got, _ := LookGH(list); got != shim {
		t.Fatalf("control: LookGH without skip = %q; want %q", got, shim)
	}
}
