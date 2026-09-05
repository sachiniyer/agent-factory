# Develop and maintain

For contributors and maintainers, not for people using `af`. This section holds
everything about working **on** the repository: the test harnesses, the gates a
pull request has to clear, the release machinery, and the design notes behind
the larger pieces.

The operating contract for this repository — how work is triaged, what runs
locally versus in CI, and the git hygiene a shared box needs — lives in
[CLAUDE.md](https://github.com/sachiniyer/agent-factory/blob/master/CLAUDE.md)
at the repository root. Read it first.

## Before opening a pull request

```bash
gofmt -l .                              # no output
go build ./...
go vet ./...
golangci-lint run --timeout=3m --fast
scripts/lint-file-length.sh
go test ./<the-package-you-changed>/... # not ./... on a shared box
```

CI runs the rest — including `go test -race ./...`, the container suites, and
the docs build — on every push.

## The pages

| Page | What it covers |
| --- | --- |
| [Container testing](container-testing.md) | Running the suite and play-tests inside docker, so real tmux servers and real daemons cannot escape. |
| [Lifecycle testing](lifecycle-testing.md) | Clean install and install → upgrade on a real machine: the bugs that need two versions to exist. |
| [Web client selftest](web-selftest.md) | The Playwright acceptance proof for the embedded web client. |
| [Manual TUI testing](tui-manual-testing.md) | `scripts/tui-driver.sh`, the self-synchronizing driver for play-testing the live TUI. |
| [Surface parity](surface-parity.md) | The drift check that keeps the TUI, web, and CLI the same product. |
| [File-length lint](file-length-lint.md) | The structural-health guard that bounds Go file length. |
| [Release process](release-process.md) | Stable and preview channels, version scheme, and how updates reach users. |
| [Release testing plan](release-testing-plan.md) | The checklist a release commit has to pass. |
| [Release notes](release-notes.md) | Curated notes for changes users need to know about. |
| [Daemon memory](daemon-memory.md) | Sizing the daemon, and why the unit's `MemoryPeak` is not its memory. |
| [TUI rewrite](../design/tui-rewrite.md) · [Agent handoff](../design/agent-handoff.md) | Accepted design notes for the larger epics. |

## Documentation itself

The site is MkDocs Material, built from `docs/` and `mkdocs.yml`:

```bash
python3 -m venv .venv-docs
.venv-docs/bin/pip install -r requirements-docs.txt
.venv-docs/bin/mkdocs serve          # live preview
.venv-docs/bin/mkdocs build --strict # what CI gates on
```

`docs/reference/cli.md` and `docs/reference/api.md` are **generated** — run
`scripts/gen-docs.sh` and commit the result rather than editing them; CI fails
on drift. Pages under the docs root are user-facing, pages under `docs/dev/` are these,
`docs/design/` holds accepted design notes, and `docs/reference/` is generated. When a page moves, add its old path to the `redirects` map in
`mkdocs.yml` so published links keep resolving.
