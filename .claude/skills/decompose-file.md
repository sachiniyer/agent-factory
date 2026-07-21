---
name: decompose-file
description: Decompose one oversized file from a fresh master branch without stacking or logic changes
user_invocable: true
---

# Decompose File

Use the #1195 Phase 3 process to split one oversized file into cohesive files.
Each PR handles exactly one file, starts fresh from current `master`, and
merges before the next file begins.

## Steps

1. **Start fresh from master** — never stack on another PR branch:
   ```bash
   git fetch origin master
   git checkout -B phase-<file> origin/master
   ```

2. **Split by responsibility** — move cohesive types, helpers, tests, or
   command groups into clearly named files. Keep package boundaries and public
   behavior unchanged.

3. **Keep it pure move** — do not make logic changes in the decomposition PR.
   Verify moved function bodies exactly match the original source. If you find
   a bug or cleanup while moving code, open a follow-up PR after the
   decomposition merges.

4. **Check for accidental stacking before pushing**:
   ```bash
   git diff --stat origin/master
   ```

   Confirm the stat lists only the original oversized file, its split files,
   and any expected `scripts/file-length-allowlist.txt` or docs changes. If
   any other file appears, the branch started from a stale base or another PR;
   redo it from current `origin/master`.

5. **Ratchet the allowlist** — reduce or remove the file's entry in
   `scripts/file-length-allowlist.txt`. Prefer removing the entry entirely
   when the split brings the file below the lint threshold.

6. **Run the gates**:
   ```bash
   gofmt -w <changed-go-files>
   golangci-lint run --timeout=3m --fast
   gofmt -l .
   go build ./...
   make test-container
   deadcode -test ./...
   scripts/lint-file-length.sh
   ```

   If the change is TUI-visible, also play-test the affected flow before
   opening the PR.

7. **Open one PR against master** — base the PR on `master`, explain that it
   is a pure decomposition, and merge it before starting the next file. The
   allowlist is a shared conflict point, so serialize this work.
