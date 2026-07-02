# Release Testing Plan

This checklist is the release gate for Agent Factory. It focuses on the
behaviors that can lose user work or leave background resources behind:
daemon coordination, tmux sessions, git worktrees, scheduled task runs,
remote hooks, and release artifacts.

## Automated Release Gate

These checks must pass on the release commit before cutting a release:

```bash
gofmt -l .
go vet ./...
go test -v -race -count=1 ./...
go build ./...
```

The PR workflow also runs lint, dependency review, CodeQL, and a build job.
Both release workflows (auto preview releases and manual stable releases —
see [release-process.md](release-process.md)) run the release preflight
before tagging or publishing artifacts. Workflow linting with
`actionlint` is part of the local release review because broken workflow
syntax or retired action runtimes can block CI/release automation.

## Local Preflight

Run this from a clean checkout of `master`:

```bash
git fetch origin --prune
git status --short --branch
go test -v -race -count=1 ./...
go vet ./...
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
git diff --check
for goos in linux darwin; do
  for goarch in amd64 arm64; do
    GOOS=$goos GOARCH=$goarch go build -o /tmp/af-$goos-$goarch .
  done
done
```

Expected result:
- Worktree is clean before and after tests.
- All tests pass.
- Cross-compiled binaries build for Linux and macOS on amd64 and arm64.

## Functional Coverage Matrix

| Area | Automated coverage | Manual release smoke |
| --- | --- | --- |
| CLI session lifecycle | `integration/cli_daemon_test.go` creates, lists, sends prompts, previews, kills, and checks tmux/worktree cleanup through the real CLI and daemon. | Run the commands in "Manual Smoke" against a temp repo. |
| Daemon coordination | Integration tests cover stale socket recovery, dead daemon restart, duplicate create races, and daemon-owned mutations. | Verify `daemon.sock` and `daemon.pid` are created in a temp `AGENT_FACTORY_HOME`, then removed or replaced after kill/restart. |
| TUI creation and sync | App E2E tests cover async creation, navigation while creating, TUI refresh seeing CLI changes, and real TUI creation with real tmux/git under the race detector. | Launch `af` in a temp repo, create one `cat` session, switch preview/terminal tabs, then kill it. |
| Git worktrees | Session/git tests cover worktree naming, collisions, linked worktrees, branch cleanup, prune before delete, and missing paths. | After kill/reset, run `git worktree list` and confirm no test worktree remains. |
| Scheduled tasks | Unit tests cover cron parsing/validation agreement, the in-daemon scheduler (registration, CRUD reload, firing), the watcher supervisor (event delivery, backoff, crash-loop breaker, rate limiting, process-group kills), target-session delivery, the legacy-unit upgrade sweep, daemon autostart unit generation, task CRUD, and task runner storage. Integration tests run the same task twice and verify `name` then `name-2`. | Add a task with a cron one minute out, confirm the daemon fires it (a session appears), then remove the task and confirm the schedule is gone via another fire window. Verify `af daemon install`/`uninstall` on Linux (systemd user service) and macOS (launchd agent) before releases that change daemon lifecycle code. |
| Remote hooks | Session and integration tests cover launch/list/import/attach/delete/terminal protocols, bad JSON, command failures, duplicate imports, and imported display-title deletion. | Configure `examples/remote-hooks` or a repo-local fake hook set, import, preview, and delete a remote session. |
| Reset/cleanup | Unit tests cover ghost sessions and worktree cleanup helpers. | With temp `AGENT_FACTORY_HOME`, create two sessions, run `af reset`, then confirm tmux sessions, worktrees, and stored instances are gone. |
| Upgrade/release artifacts | Unit tests cover binary download (success, non-200, stalled body, stalled headers), archive extraction, and daemon restart after binary swap; CI builds Linux/macOS artifacts. | Download the release tarball for the host platform, unpack it, run `af version`, and test `af upgrade` from the previous release binary. Run `install.sh` into a temp `AF_INSTALL_DIR` and confirm it fetches the latest release and `af version` reports the new tag. |
| UI rendering | UI tests cover sidebar/menu/task pane/preview/terminal layout and overlay behavior. | Open the TUI in a narrow terminal and a normal terminal, verify text does not overlap, and check help/confirmation overlays. |

## Manual Smoke

Use an isolated config directory so release testing cannot touch real user
sessions:

```bash
tmp_home=$(mktemp -d)
tmp_repo=$(mktemp -d)
export AGENT_FACTORY_HOME="$tmp_home"

git -C "$tmp_repo" init
git -C "$tmp_repo" config user.email test@example.com
git -C "$tmp_repo" config user.name "Release Test"
git -C "$tmp_repo" commit --allow-empty -m init

go build -o /tmp/af-release-smoke .
/tmp/af-release-smoke version
/tmp/af-release-smoke debug

/tmp/af-release-smoke sessions --repo "$tmp_repo" create --name smoke --program claude
/tmp/af-release-smoke sessions --repo "$tmp_repo" list
/tmp/af-release-smoke sessions --repo "$tmp_repo" send-prompt smoke "release-smoke"
/tmp/af-release-smoke sessions preview smoke
/tmp/af-release-smoke sessions --repo "$tmp_repo" kill smoke
/tmp/af-release-smoke sessions --repo "$tmp_repo" list
```

Expected result:
- `preview` contains `release-smoke` before kill.
- `list` is empty after kill.
- No `af_` tmux session or generated worktree remains for the smoke repo.

## Release Artifact Verification

After the release workflow publishes (`--latest` only sees the stable
channel; verify a preview release by its `vX.Y.Z-preview-N` tag instead):

```bash
gh release view --repo sachiniyer/agent-factory --latest   # or: gh release view <tag>
gh release download --repo sachiniyer/agent-factory --latest --pattern 'agent-factory-*.tar.gz' --dir /tmp/af-release
for archive in /tmp/af-release/*.tar.gz; do
  tar tzf "$archive"
done
```

Expected result:
- Artifacts exist for `linux-amd64`, `linux-arm64`, `darwin-amd64`, and
  `darwin-arm64`.
- Each archive contains one executable named `agent-factory`.
- The host-platform binary runs `version` and reports the new tag.

Then confirm the no-Go install path fetches the freshly published release:

```bash
AF_INSTALL_DIR="$(mktemp -d)" sh install.sh
```

Expected result:
- `install.sh` downloads `agent-factory-<os>-<arch>.tar.gz` via the
  `releases/latest/download/...` redirect, installs `af`, and the printed
  `af version` reports the newest stable tag (the redirect never serves
  preview prereleases; pin one explicitly with `install.sh --version <tag>`).

## Go/No-Go Criteria

Do not cut or publish a release if any of these are true:
- Any required CI check is failing or pending.
- Local `go test -v -race -count=1 ./...` fails.
- Manual smoke leaves behind tmux sessions, worktrees, or stale storage.
- The release workflow cannot build all four artifacts.
- There is an open issue or PR marked as a release blocker.

Non-blocking, known limits:
- Real Claude/Aider/Codex/Gemini interactive behavior is not fully automated;
  tests use a fake wrapper script (routed via `program_overrides`) and fake backends for deterministic runs.
- The `af daemon install` autostart unit (systemd user service / launchd
  agent) should be manually smoke-tested on its native OS before a release
  that changes daemon lifecycle code.
- `af upgrade` requires a published release asset, so end-to-end upgrade is a
  post-publish verification step.
