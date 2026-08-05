---
name: create-pr
description: Create a pull request for the current branch
user_invocable: true
---

# Create Pull Request

Create a PR for the current branch against `master`.

## Steps

1. **Pre-flight checks** — before creating the PR, run these and fix any issues:
   ```bash
   gofmt -l .                    # fix any output with: gofmt -w <file>
   go build ./...
   golangci-lint run --timeout=3m --fast
   scripts/lint-file-length.sh
   go test ./<only the package you changed>/...   # skip if it is daemon/ or app/

   # Do NOT run make test-container as a routine gate — CI runs
   # `go test -race ./...` on every push, and a local container run rebuilds the
   # whole Go tree, which takes the shared box down when sessions do it in
   # parallel. Never bare `go test ./...` on the host. If your change is in
   # daemon/ or app/, run NO tests for it locally — they spawn real af daemons
   # and drive real tmux next to live sessions. Push and let CI test it.
   ```

2. **Review changes** — examine the diff against master:
   ```bash
   git diff master...HEAD
   git log master..HEAD --oneline
   ```

3. **Push the branch** (if not already pushed):
   ```bash
   git push -u origin HEAD
   ```

4. **Create the PR** using the repo's PR template structure:
   ```bash
   gh pr create --title "<concise title>" --body "$(cat <<'EOF'
   ## Summary

   <1-3 bullet points describing what changed and why>

   Closes #<issue>

   <!-- If this PR closes nothing, write "no issue" rather than deleting the line. -->

   ## Test Plan

   - [x] `golangci-lint run --timeout=3m --fast` passes
   - [x] `gofmt -l .` produces no output
   - [x] `go build ./...` passes
   - [x] `go test` on the changed package passes (CI runs the full matrix)
   - [x] `scripts/lint-file-length.sh` passes
   - [x] `deadcode` left to CI (whole-program analysis; the Lint job runs it)
   - [ ] Manually tested in TUI (if applicable)
   EOF
   )"
   ```

   - Keep the title under 70 characters
   - Mark test plan items as checked only if you actually ran them
   - Add `Manually tested in TUI` only if the change affects UI behavior

5. **Return the PR URL** to the user.
