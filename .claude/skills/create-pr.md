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

   # Generated-artifact drift — only when the diff touched a generator input
   # (commands/ or api/ Cobra defs, daemon/httproutes.go, session/ usage text,
   # design/, web/src shells, app/testdata/recovery goldens, or the
   # generators). The porcelain path list mirrors docs.yml's generated set.
   scripts/gen-docs.sh
   git status --porcelain -- docs/reference plugins .agents .claude-plugin \
       web/src/tokens.css web/src/index.html web/src/manifest.webmanifest \
       ui/theme docs/stylesheets/tokens.css docs/design/style-guide.md \
       docs/design/interface-design.md \
       docs/assets/recovery/tui-model-driver   # must be empty

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
   - [x] `scripts/gen-docs.sh` run; generated paths clean — or no generator input touched
   - [x] `deadcode` left to CI (whole-program analysis; the Lint job runs it)
   - [ ] Manually tested in TUI (if applicable)
   EOF
   )"
   ```

   - Keep the title under 70 characters
   - Mark test plan items as checked only if you actually ran them
   - Add `Manually tested in TUI` only if the change affects UI behavior

5. **Return the PR URL** to the user.
