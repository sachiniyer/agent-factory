//go:build linux

package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// The throwaway proof's socket reader, on real sockets this process holds
// (#7536): a named listener is reported by its path, an abstract one with its
// Abstract flag (spelled "@name"), and an unbound socketpair not at all. These
// are the shapes tmuxServerSocketPrivate decides on.
func TestUnixSocketsOfReportsAbstractAndSkipsUnbound(t *testing.T) {
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

	socks, ok := unixSocketsOf(os.Getpid())
	if !ok {
		t.Fatal("the per-socket read gave no evidence for this process's own sockets")
	}
	var sawNamed, sawAbstract bool
	for _, s := range socks {
		if s.Path == "" {
			t.Fatalf("an unbound socket was reported: %+v", socks)
		}
		if s.Path == named && !s.Abstract {
			sawNamed = true
		}
		if s.Path == abstract && s.Abstract {
			sawAbstract = true
		}
	}
	if !sawNamed {
		t.Errorf("the named listener %s is missing from %+v", named, socks)
	}
	if !sawAbstract {
		t.Errorf("the abstract listener %s is missing, or lacks its Abstract flag, in %+v: it must be reported, so it fails the proof", abstract, socks)
	}
}

// The proof itself on those shapes: every socket a named path inside the
// private tmux dir proves the server private. An abstract socket fails it on
// its flag, before any path arithmetic, whatever its name spells (#7543 review
// R2): "@/../../…/<canonTmux>/x" resolves, against this process's working
// directory, to a path inside the dir. So does a relative path, and a named
// socket outside, and no socket evidence at all.
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
	inside := proctree.UnixSocket{Path: filepath.Join(canonTmux, "default"), Listening: true}
	// Enough ".." to climb from any working directory to the root, then into
	// the private dir: the spelling that passed when "@" alone was the guard.
	climb := "@/" + strings.Repeat("../", 64) + strings.TrimPrefix(canonTmux, "/") + "/x"
	if c, err := canonicalDir(climb); err != nil || !strings.HasPrefix(c, canonTmux+"/") {
		t.Fatalf("fixture: %q must canonicalise inside %s for this row to mean anything, got %q (%v)", climb, canonTmux, c, err)
	}
	for _, tc := range []struct {
		name  string
		socks []proctree.UnixSocket
		ok    bool
		want  bool
	}{
		{"every socket a named path inside the private dir", []proctree.UnixSocket{inside}, true, true},
		{"an abstract socket beside them", []proctree.UnixSocket{inside, {Path: "@some-abstract-name", Abstract: true}}, true, false},
		{"an abstract socket whose name climbs into the private dir", []proctree.UnixSocket{inside, {Path: climb, Abstract: true}}, true, false},
		{"an abstract socket whose name spells a path inside the dir", []proctree.UnixSocket{inside, {Path: inside.Path, Abstract: true}}, true, false},
		{"a relative path", []proctree.UnixSocket{inside, {Path: "default"}}, true, false},
		{"a named socket outside", []proctree.UnixSocket{inside, {Path: "/tmp/elsewhere.sock"}}, true, false},
		{"no socket evidence", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			throwawayUnixSocketsFn = func(int) ([]proctree.UnixSocket, bool) { return tc.socks, tc.ok }
			if got := tmuxServerSocketPrivate(4242, canonTmux); got != tc.want {
				t.Fatalf("tmuxServerSocketPrivate = %v, want %v for %+v", got, tc.want, tc.socks)
			}
		})
	}
}
