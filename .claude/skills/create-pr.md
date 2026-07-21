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
   golangci-lint run --timeout=3m --fast
   gofmt -l .                    # fix any output with: gofmt -w <file>
   go build ./...
   make test-container           # never run bare go test ./... on a shared host
   deadcode -test ./...
   scripts/lint-file-length.sh
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

   ## Test Plan

   - [x] `golangci-lint run --timeout=3m --fast` passes
   - [x] `gofmt -l .` produces no output
   - [x] `go build ./...` passes
   - [x] `make test-container` passes
   - [x] `deadcode -test ./...` produces no output
   - [x] `scripts/lint-file-length.sh` passes
   - [ ] Manually tested in TUI (if applicable)
   EOF
   )"
   ```

   - Keep the title under 70 characters
   - Mark test plan items as checked only if you actually ran them
   - Add `Manually tested in TUI` only if the change affects UI behavior

5. **Return the PR URL** to the user.
