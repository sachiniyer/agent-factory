package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHomeSymlinkEntersProcessRelativeProcfs_NestedSymlinkTarget(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	dir := t.TempDir()
	// link2 -> /proc/self/cwd/state; link1 -> link2/..
	// A daemon using link1 resolves link2 first (/proc/self/cwd/state), then ..,
	// serving its own cwd. The cleaned chain walk collapses link2/.. to the dir
	// and never follows link2, so the procfs indirection is missed.
	if err := os.Symlink("/proc/self/cwd/state", filepath.Join(dir, "link2")); err != nil {
		t.Fatalf("symlink link2: %v", err)
	}
	// Build the home WITHOUT filepath.Join, which would clean link1's target.
	link1 := filepath.Join(dir, "link1")
	if err := os.Symlink("link2/..", link1); err != nil {
		t.Fatalf("symlink link1: %v", err)
	}
	if !homeSymlinkEntersProcessRelativeProcfs(link1) {
		t.Errorf("homeSymlinkEntersProcessRelativeProcfs(%q) = false; want true — link1 -> link2/.. where link2 -> /proc/self/cwd/state resolves in the daemon's frame to its cwd, but the cleaned chain walk collapsed link2/.. and missed the procfs indirection", link1)
	}
}
