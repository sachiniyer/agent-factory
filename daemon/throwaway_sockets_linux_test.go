//go:build linux

package daemon

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The throwaway proof's socket reader, on real sockets this process holds
// (#7536): a named listener is reported by its path, an abstract one as
// "@name", and an unbound socketpair not at all. These are the shapes
// tmuxServerSocketPrivate decides on, kept as they were before the shared
// reader.
func TestUnixSocketPathsOfReportsAbstractAndSkipsUnbound(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "af-7536-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	named := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", named)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	abstract := "@af-7536-" + filepath.Base(dir)
	abs, err := net.Listen("unix", abstract)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = abs.Close() })
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(pair[0]); _ = syscall.Close(pair[1]) })

	paths, ok := unixSocketPathsOf(os.Getpid())
	if !ok {
		t.Fatal("the per-socket read gave no evidence for this process's own sockets")
	}
	has := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			t.Fatalf("an unbound socket was reported with an empty path: %q", paths)
		}
		has[p] = true
	}
	if !has[named] {
		t.Errorf("the named listener %s is missing from %q", named, paths)
	}
	if !has[abstract] {
		t.Errorf("the abstract listener %s is missing from %q: it must be reported, so it fails the proof", abstract, paths)
	}
}

// The proof itself on those shapes: every reported path inside the private
// tmux dir proves the server private; an abstract socket beside them, which no
// directory contains, fails it, as does no socket evidence at all.
func TestTmuxServerSocketPrivateOnAbstractAndNamedSockets(t *testing.T) {
	canonTmux, err := canonicalDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := throwawaySeams()
	defer throwawayRestoreSeams(old)
	throwawayIsTmuxServerFn = func(int) bool { return true }
	throwawaySameRootFn = func(int) bool { return true }
	throwawayUIDFn = func(int) (int, bool) { return os.Getuid(), true }
	inside := filepath.Join(canonTmux, "default")
	for _, tc := range []struct {
		name  string
		paths []string
		ok    bool
		want  bool
	}{
		{"every path inside the private dir", []string{inside}, true, true},
		{"an abstract socket beside them", []string{inside, "@some-abstract-name"}, true, false},
		{"a named socket outside", []string{inside, "/tmp/elsewhere.sock"}, true, false},
		{"no socket evidence", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			throwawayUnixSocketPathsFn = func(int) ([]string, bool) { return tc.paths, tc.ok }
			if got := tmuxServerSocketPrivate(4242, canonTmux); got != tc.want {
				t.Fatalf("tmuxServerSocketPrivate = %v, want %v for %q", got, tc.want, tc.paths)
			}
		})
	}
}
