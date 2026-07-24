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

// globMetacharTree is unreadableTree's adversarial twin: the unreadable file's
// BASENAME contains shell-glob metacharacters, and a readable sibling sits next
// to it whose name the same characters would match when read as a pattern.
//
// `secret[1].env` unreadable + `secret1.env` readable is the minimal fixture
// that separates "this path is excluded" from "this pattern is excluded", and
// it catches both halves of the same mistake at once:
//
//   - As a pattern, `./secret[1].env` is a character class matching `secret1`,
//     so it does NOT match the literal file it came from — the unreadable file
//     stays in the archive, tar opens it, and the run dies exactly as it did
//     before #2432 was fixed.
//   - That same pattern DOES match the readable sibling, which is dropped from
//     the copy with no error and exit 0 — a source file that silently is not
//     there, surfacing later as a compile error far from its cause.
//
// The `*` case is included too because it fails the same way without needing a
// character class, which is the form most likely to appear in a real tree.
func globMetacharTree(t *testing.T, private, readable string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("copy_src_tree targets the Linux container image (GNU find)")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can read any file; the unreadable fixture is meaningless")
	}

	src := t.TempDir()
	for _, name := range []string{"keep.txt", readable} {
		if err := os.WriteFile(filepath.Join(src, name), []byte("REAL SOURCE\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	secret := filepath.Join(src, private)
	if err := os.WriteFile(secret, []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", private, err)
	}
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", private, err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o600) })
	return src
}

// The P2: the skip list must match PATHS, never patterns. `--exclude-from` reads
// each line as a glob, which fails in two OPPOSITE directions depending on which
// metacharacter the unreadable name happens to contain — so each gets its own
// case, because a fix for one alone leaves the other live and a single combined
// test would only ever report whichever fired first.
func TestCopySrcTree_GlobMetacharInUnreadableName(t *testing.T) {
	// A character class does not match the literal text it was built from:
	// `./secret[1].env` as a pattern matches `secret1.env` and NOT
	// `secret[1].env`. So the unreadable file is not excluded at all, tar opens
	// it, and the run dies — #2432 reintroduced, loudly.
	t.Run("class pattern misses its own path and aborts the run", func(t *testing.T) {
		src := globMetacharTree(t, "secret[1].env", "secret1.env")

		dest, out, err := runCopySrcTree(t, src)
		if err != nil {
			t.Fatalf("copy_src_tree aborted on an unreadable name containing a glob metacharacter — "+
				"the exclusion did not match the literal path, so tar still opened it (#2432 reintroduced): %v\n%s", err, out)
		}
		assertCopiedAndSkipped(t, dest, out, "secret1.env", "secret[1].env")
	})

	// The quiet one, and the worse one. `./star*.key` DOES match itself, so the
	// run exits 0 and looks fine — while the same pattern also swallows the
	// readable `starindex.key`, which is simply not in the copy. No error, no
	// warning, a source file gone. This is the "missing source -> mystery compile
	// error" outcome --ignore-failed-read was rejected for, arriving by another
	// route.
	t.Run("star pattern silently swallows a readable sibling", func(t *testing.T) {
		src := globMetacharTree(t, "star*.key", "starindex.key")

		dest, out, err := runCopySrcTree(t, src)
		if err != nil {
			t.Fatalf("copy_src_tree: %v\n%s", err, out)
		}
		assertCopiedAndSkipped(t, dest, out, "starindex.key", "star*.key")
	})
}

// assertCopiedAndSkipped pins the whole contract for one fixture: the readable
// files arrived, the unreadable one did not, and the skip was reported.
func assertCopiedAndSkipped(t *testing.T, dest, out, readable, private string) {
	t.Helper()
	for _, rel := range []string{readable, "keep.txt"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Fatalf("readable %s is missing from the copy: an unreadable sibling's name was treated as a "+
				"glob and swallowed it — a source file silently absent, with exit 0 and nothing on stderr: %v\n%s",
				rel, err, out)
		}
	}
	// Still excluded, so the fix is literal matching rather than a blanket
	// "copy everything and hope".
	if _, err := os.Stat(filepath.Join(dest, private)); err == nil {
		t.Fatalf("%s was copied; the fixture is not actually unreadable and this test proves nothing", private)
	}
	// A metacharacter in the name must not cost the user the report either.
	if !strings.Contains(out, private) {
		t.Fatalf("skipped path %q was not named in the output:\n%s", private, out)
	}
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
