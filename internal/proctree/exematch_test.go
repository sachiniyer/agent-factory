package proctree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// The verdict table is the contract: a conclusive image read decides, a
// refused one is only ever Unknown, and a dead entry is Gone — never the
// other way around, because the safe direction is a candidate too many,
// not a live owner dropped.
func TestExeMatchesImageVerdictTable(t *testing.T) {
	ref := ExeImage{dev: 8, ino: 42}
	other := ExeImage{dev: 8, ino: 43}
	for _, tc := range []struct {
		name string
		id   ExeImage
		err  error
		want ExeMatch
	}{
		{"same image", ref, nil, ExeMatchSame},
		{"a different inode is conclusive", other, nil, ExeMatchOther},
		{"process entry gone", ExeImage{}, syscall.ENOENT, ExeMatchGone},
		{"process gone", ExeImage{}, syscall.ESRCH, ExeMatchGone},
		{"read denied keeps it unproven", ExeImage{}, syscall.EACCES, ExeMatchUnknown},
		{"operation not permitted keeps it unproven", ExeImage{}, syscall.EPERM, ExeMatchUnknown},
		{"any other failure is unproven, never disproven", ExeImage{}, syscall.EIO, ExeMatchUnknown},
		{"a wrapped denial is still a denial", ExeImage{}, errors.Join(errors.New("read failed"), syscall.EACCES), ExeMatchUnknown},
		{"a wrapped gone is still gone", ExeImage{}, errors.Join(errors.New("read failed"), syscall.ENOENT), ExeMatchGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := exeMatchRead
			exeMatchRead = func(int) (ExeImage, error) { return tc.id, tc.err }
			t.Cleanup(func() { exeMatchRead = prev })
			if got := ExeMatchesImage(4242, ref); got != tc.want {
				t.Errorf("ExeMatchesImage(4242, ref) = %v, want %v", got, tc.want)
			}
		})
	}
}

// exeReference resolves a path's ExeImage or skips where there is no
// kernel exe backend to compare against.
func exeReference(t *testing.T, path string) ExeImage {
	t.Helper()
	img, err := ExeImageOf(path)
	if err != nil {
		t.Skipf("no exe-identity backend here: %v", err)
	}
	return img
}

// The identity follows the inode, not the name: a process is Same against
// its own running image even though the comparison never reads a name, and
// Other against a file that merely shares the directory.
func TestExeMatchesImageAgainstTheLiveProcess(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		t.Skipf("EvalSymlinks: %v", err)
	}
	selfImg := exeReference(t, self)

	if got := ExeMatchesImage(os.Getpid(), selfImg); got != ExeMatchSame {
		t.Errorf("ExeMatchesImage(self, own image) = %v, want ExeMatchSame", got)
	}

	stranger := filepath.Join(t.TempDir(), "stranger")
	if err := os.WriteFile(stranger, []byte("not an executable anyone runs"), 0o600); err != nil {
		t.Fatalf("write stranger: %v", err)
	}
	if got := ExeMatchesImage(os.Getpid(), exeReference(t, stranger)); got != ExeMatchOther {
		t.Errorf("ExeMatchesImage(self, unrelated file) = %v, want ExeMatchOther — "+
			"a readable different image is the conclusive answer, not 'unknown'", got)
	}
}

// The reference resolves through a symlink to the same inode: a renamed
// spelling of the tmux binary (a symlink, a hardlink, an exec -a argv[0])
// is the SAME image, which is what the candidacy check relies on.
func TestExeImageOfFollowsTheNameToTheInode(t *testing.T) {
	target := filepath.Join(t.TempDir(), "real-tmux")
	if err := os.WriteFile(target, []byte("image"), 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(t.TempDir(), "renamed-tmux")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if exeReference(t, link) != exeReference(t, target) {
		t.Error("a symlink resolved to a different image than its target — " +
			"renamed spellings of the binary must share the exe identity")
	}
}

// A reaped process has no image to read: the verdict is Gone, not
// Unknown, because a dead pid owns nothing the caller is asking after.
func TestExeMatchesImageReportsAGoneProcess(t *testing.T) {
	exeReference(t, os.Args[0]) // skip early where the backend is absent
	c := exec.Command(os.Args[0], "-test.run=^$")
	if err := c.Run(); err != nil {
		t.Fatalf("fixture child: %v", err)
	}
	if got := ExeMatchesImage(c.Process.Pid, ExeImage{}); got != ExeMatchGone {
		t.Errorf("ExeMatchesImage(reaped pid) = %v, want ExeMatchGone", got)
	}
}
