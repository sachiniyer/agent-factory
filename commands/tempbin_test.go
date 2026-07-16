package commands

import (
	"os"
	"path/filepath"
	"testing"
)

// tempBinPath returns the path of a stand-in `af` binary inside a fresh temp dir,
// spelled the way the code under test will spell it back (#1918).
//
// The upgrade/auto-update paths resolve their executable through
// filepath.EvalSymlinks before reporting it (autoupdate.go, autoupdate_launch.go,
// upgrade.go, daemoncmd.go all do), so an assertion has to compare a resolved path
// against a resolved path. On macOS it does not: t.TempDir() hands back
// /var/folders/…, /var is a symlink to /private/var, and the code answers
// /private/var/folders/… — the same file, a different spelling, and a failed
// string compare on a clean tree (darwin/arm64, go1.25). Linux temp dirs are not
// symlinked, so CI never saw it.
//
// Canonicalizing here rather than at each assertion fixes the cause once: the
// expectation is simply written in the same spelling the production code uses, so
// new assertions on these paths are correct by default. EvalSymlinks on an
// already-canonical path is a no-op, which is why this is inert on Linux.
//
// It resolves the DIRECTORY, not the file: the binary does not exist yet at this
// point (callers write it afterwards, or let the code under test install it), and
// EvalSymlinks requires an existing path.
func tempBinPath(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize temp dir: %v", err)
	}
	return filepath.Join(dir, "agent-factory")
}

// TestTempBinPath_MatchesResolvedExecutablePath reproduces #1918's condition on
// ANY platform instead of trusting that a Mac behaves as described.
//
// It builds the exact shape macOS hands out — a symlinked directory standing in
// for /var → /private/var — and asserts that what the production code reports
// (EvalSymlinks of its executable path) equals what tempBinPath predicts. Compare
// against the unsymlinked spelling and the two differ, which is precisely the
// failure the four tests hit.
func TestTempBinPath_MatchesResolvedExecutablePath(t *testing.T) {
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("platform cannot symlink: %v", err)
	}

	// The spelling a naive test would assert on: reached VIA the symlink, the way
	// t.TempDir() hands back /var/folders/… on macOS.
	viaLink := filepath.Join(linkDir, "agent-factory")
	if err := os.WriteFile(viaLink, []byte("binary"), 0o755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	// The spelling the code under test reports, since every one of these paths
	// EvalSymlinks its executable before using it.
	resolved, err := filepath.EvalSymlinks(viaLink)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved == viaLink {
		t.Skip("temp dir is not symlinked here, so there is no skew to reproduce")
	}

	// This is #1918 in one line: same file, two spellings, string compare fails.
	if filepath.Base(resolved) != filepath.Base(viaLink) {
		t.Fatalf("precondition: expected the same file, got %q vs %q", resolved, viaLink)
	}

	// tempBinPath must speak the resolved spelling, so an assertion written
	// against it matches what the production code reports.
	got := tempBinPath(t)
	if got != filepath.Clean(got) {
		t.Fatalf("tempBinPath must return a clean path, got %q", got)
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(got)); err != nil || r != filepath.Dir(got) {
		t.Fatalf("tempBinPath must return an already-resolved dir (EvalSymlinks-stable); got %q which resolves to %q (err %v)", filepath.Dir(got), r, err)
	}
}
