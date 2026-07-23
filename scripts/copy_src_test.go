package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The container entrypoints mirror the read-only /src bind mount into a
// writable tree before building. That mount carries the HOST user's ownership
// while the image runs as `dev` (uid 1000), so a mode-0600 file belonging to a
// developer whose uid is not 1000 is unreadable inside the container — and
// `tar -c` exiting non-zero on ONE such file used to take the whole run down
// before a single test compiled (#2432).
//
// The files that trigger it are working-directory debris — .env, .netrc, editor
// state, agent tool configs — never repo content under test. An external
// contributor hitting this on their own .env learns only that the harness is
// broken.

// runCopySrcTree sources copy-src.sh and copies src into a fresh dest,
// returning dest and the combined output.
func runCopySrcTree(t *testing.T, src string, extraArgs ...string) (string, string, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "work")
	helper := filepath.Join(repoRoot(t), "scripts", "container", "copy-src.sh")
	// `set -euo pipefail` is not decoration: every entrypoint that sources this
	// helper sets it, and it is the whole reason one unreadable file was fatal —
	// without pipefail the failing `tar -c` is masked by the succeeding `tar -x`
	// at the other end of the pipe, and the copy merely comes out incomplete.
	// A harness that omitted it would be strictly weaker than production and
	// would watch this bug pass.
	script := "set -euo pipefail\n. " + helper + "\ncopy_src_tree " + src + " " + dest
	for _, arg := range extraArgs {
		script += " " + arg
	}
	cmd := exec.Command("bash", "-c", script)
	out, err := cmd.CombinedOutput()
	return dest, string(out), err
}

// unreadableTree builds a source tree holding one ordinary file and one the
// current user cannot read, which is what a private file on the host mount
// looks like from inside the container.
func unreadableTree(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		// copy_src_tree uses GNU `find -readable`, and it only ever runs inside
		// the Linux testbox image. Skipping honestly beats asserting a
		// portability claim this helper does not make.
		t.Skip("copy_src_tree targets the Linux container image (GNU find)")
	}
	if os.Geteuid() == 0 {
		// Root reads through any mode bits, so the unreadable file this test
		// depends on would simply be readable and prove nothing.
		t.Skip("root can read any file; the unreadable fixture is meaningless")
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "keep.txt"), []byte("repo content\n"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(src, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir pkg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "pkg", "nested.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatalf("write nested.go: %v", err)
	}
	private := filepath.Join(src, ".env")
	if err := os.WriteFile(private, []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.Chmod(private, 0o000); err != nil {
		t.Fatalf("chmod .env: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(private, 0o600) })
	return src
}

// TestCopySrcTree_UnreadableFileDoesNotAbortTheCopy is the #2432 regression: one
// unreadable path must cost that path, not the entire run.
func TestCopySrcTree_UnreadableFileDoesNotAbortTheCopy(t *testing.T) {
	src := unreadableTree(t)

	dest, out, err := runCopySrcTree(t, src)
	if err != nil {
		t.Fatalf("copy_src_tree failed on a tree containing one unreadable file: %v\n%s", err, out)
	}

	// The point of the copy: repo content still arrives, at every depth.
	for _, rel := range []string{"keep.txt", filepath.Join("pkg", "nested.go")} {
		if _, statErr := os.Stat(filepath.Join(dest, rel)); statErr != nil {
			t.Fatalf("%s missing from the copy — the skip must cost only the unreadable path: %v", rel, statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dest, ".env")); statErr == nil {
		t.Fatal(".env was copied; the fixture is not actually unreadable and this test proves nothing")
	}
}

// A skip that is not reported is indistinguishable from a file that was never
// there, which is why --ignore-failed-read was the wrong tool: it drops files
// silently and turns a missing source into a mystery compile error. Whatever is
// dropped has to be named, with the reason, above the failure it might cause.
func TestCopySrcTree_NamesEverySkippedPath(t *testing.T) {
	src := unreadableTree(t)

	_, out, err := runCopySrcTree(t, src)
	if err != nil {
		t.Fatalf("copy_src_tree: %v\n%s", err, out)
	}

	if !strings.Contains(out, ".env") {
		t.Fatalf("the skipped path was not named in the output:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "cannot read") {
		t.Fatalf("output does not explain WHY the path was skipped:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "re-run") {
		t.Fatalf("output does not tell the user what to do about it:\n%s", out)
	}
}

// The ordinary case must stay silent: a clean tree has nothing to report, and a
// harness that warns on every run trains people to ignore it.
func TestCopySrcTree_SaysNothingWhenEverythingIsReadable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("copy_src_tree targets the Linux container image (GNU find)")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "keep.txt"), []byte("repo content\n"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}

	dest, out, err := runCopySrcTree(t, src)
	if err != nil {
		t.Fatalf("copy_src_tree: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("a fully readable tree must copy silently; got:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "keep.txt")); statErr != nil {
		t.Fatalf("keep.txt missing from the copy: %v", statErr)
	}
}

// Extra tar arguments still apply — web-selftest-entry.sh passes its own
// --exclude flags through this same function, and losing them would drag
// node_modules into every web-selftest run.
func TestCopySrcTree_HonorsExtraExcludes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("copy_src_tree targets the Linux container image (GNU find)")
	}
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "web", "node_modules"), 0o755); err != nil {
		t.Fatalf("mkdir node_modules: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "web", "node_modules", "junk.js"), []byte("//\n"), 0o644); err != nil {
		t.Fatalf("write junk.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "web", "app.ts"), []byte("export {}\n"), 0o644); err != nil {
		t.Fatalf("write app.ts: %v", err)
	}

	dest, out, err := runCopySrcTree(t, src, "--exclude=web/node_modules")
	if err != nil {
		t.Fatalf("copy_src_tree: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "web", "app.ts")); statErr != nil {
		t.Fatalf("web/app.ts missing from the copy: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "web", "node_modules")); statErr == nil {
		t.Fatal("web/node_modules was copied despite the caller's --exclude")
	}
}
