# Fail-first probe runs

For contributors who must show that a new test fails without their fix, when
the test lives in a package that cannot run on a shared box. That covers
`daemon/`, `app/` and `integration/`. A probe runs the Linux `Test` job alone,
on the packages and tests you name, instead of the whole PR Validation workflow.

Read [Container testing](container-testing.md) first. It covers what may run
locally. A probe is for the part that may not.

## Why a probe and not a full run

A full PR Validation run takes 50–60 minutes, and only a few run at once. It
runs lint, both systemd lanes, the macOS suite, the web checks and the
performance baselines. A fail-first answer comes from one job, the Linux
`Test`, so a full run spends everything else in the queue on nothing, and the
macOS runner is the scarcest of them. A probe skips all of it.

## The recipe

Work from your PR branch, with the fix and the new tests committed.

```bash
N=4563                                  # your issue number
git switch -c "probe/$N-failfirst"      # a throwaway branch, never your PR branch
```

Take the fix out and keep the tests. Put every mutation you want to prove in
this one commit — see [one run carries every mutation](#one-run-carries-every-mutation).
Then confirm each one actually changed the file, since an edit that did not
apply produces a green run that looks like "the test does not catch this":

```bash
git diff --stat HEAD                    # every file you meant to mutate is listed
go build ./... && go vet ./...          # a mutation that does not compile proves nothing;
                                        # vet compiles the tests without running them
git commit -am "probe: revert the fix for #$N"
git push -u origin "probe/$N-failfirst" # no PR
```

Dispatch the probe. Name the packages and a `-run` regex that matches exactly
the tests you are proving:

```bash
gh workflow run pr.yml --ref "probe/$N-failfirst" \
  -f probe=true \
  -f packages='./daemon/' \
  -f run='^(TestFooRejectsBar|TestFooKeepsBaz)$'
```

Find and follow the run. It takes a few seconds to appear:

```bash
gh run list --workflow pr.yml --branch "probe/$N-failfirst" --event workflow_dispatch --limit 1
gh run watch <run-id>
```

Read the result, then delete the branch:

```bash
gh run view <run-id> --web              # the job summary lists each failing assertion line
gh run view <run-id> --log-failed       # the full output around each one
git push origin --delete "probe/$N-failfirst"
git switch - && git branch -D "probe/$N-failfirst"
```

The job that ran is named `Test (probe)`. Every other job shows as skipped, and
most are named by a bare expression such as
`inputs.probe && 'Lint (skipped by probe)' || 'Lint'`. That is expected: see
[how a probe stays out of the gates](#how-a-probe-stays-out-of-the-gates).

## The inputs

| Input | Meaning |
| --- | --- |
| `probe` | `true` runs the probe. Without it the dispatch is a full PR Validation run and the other two inputs are ignored. |
| `packages` | Space-separated `go test` packages, each starting with `./` and staying inside the module, such as `./daemon/` or `./session/...`. Empty means `./...`. |
| `run` | A `go test -run` regex on one line. Empty runs every test in those packages. |

The probe uses the same `go test` flags as the real Linux run, `-v -race -count=1`
and the same `-timeout`, so it fails the way that run would.

Anything else is refused before `go test` starts: a package that does not start
with `./`, uses a character other than a letter, a digit, `.`, `_`, `-` or `/`,
or has a `..` segment, and an input with a line break.

Two outcomes are refused or flagged because they read as a verdict when they are
not one:

- **A green run that ran no test fails.** `go test -run` exits 0 when the regex
  matches nothing, which would look exactly like "the test did not catch the
  mutation".
- **A skipped test raises a warning.** A skipped test cannot fail, so check the
  log before reading green as "not caught". Tests gated on an environment
  variable skip here the same way they do in the real run.

## Rules

### One run carries every mutation

Put every mutation in the one probe commit and dispatch once. Each failing
assertion names its own `file:line`, so one run proves each test red on its own
terms. For a fix with several call sites, the cheapest shape is usually "fix
present, call sites reverted": each test fails at its own assertion.

Split into a second probe only when two mutations reach the same test function,
because the first `t.Fatal` hides the second.

### Failures are attributed by line

Read the failure's `file:line` and the source line it names, not the test's
name. A test with several assertions fails at the first one it reaches, which
is often a cheap precondition rather than the property in its name. The job
summary lists each failing test, preceded by the `file:line` lines it logged.

### There is no green probe

The PR's own CI is the green run: it runs the same tests with the fix in place.
A second probe of the fixed tree costs a run and proves nothing more.

### One probe per branch, and the branch is deleted afterwards

Probes on one branch share a concurrency group, so a second dispatch cancels
the first. Cancelled is not a verdict: dispatch again. A push cancels nothing,
because a probe branch has no PR, and a running probe keeps testing the commit
it started on. Dispatch again after pushing a changed mutation, and delete the
branch once you have the result.

### Only what the Linux `Test` job covers

A probe runs the Linux `Test` job's `go test` and nothing else. It cannot prove
tests that only run in another lane: the systemd lifecycle tests
(`AF_SYSTEMD_LIFECYCLE_TEST`), the macOS suite, the web checks, or the
performance baselines. For those, dispatch without `-f probe=true` to get the
full run.

## How a probe stays out of the gates

A probe can run on any branch, including a PR's head, and every job in
`pr.yml` reports a check run on that commit, skipped jobs included. The
required checks are `Build` and `Lint`. Auto Gate reads them by exact name and
takes the newest, and a repository ruleset counts a skipped check as passing.
A probe that reported a `Build` could therefore pass a PR whose real run failed,
or block one whose real run passed. `pr.yml` prevents that three ways:

- Every job that runs on every PR has a name like
  `${{ inputs.probe && 'Build (skipped by probe)' || 'Build' }}`. On a PR it
  evaluates to `Build`. In a probe the job is skipped, and GitHub does not
  evaluate a skipped job's name, so the row is named by the expression itself,
  which matches no check. `Test` runs in a probe and reads `Test (probe)`.
- A probe has its own concurrency group, `probe-<ref>`. It cannot cancel a PR
  run or Auto Gate's recovery dispatch on the same branch.
- Auto Gate's recovery dispatch passes no inputs, so it still gets the full run.

`.github/scripts/pr-probe.test.js` pins all of this in the required `Lint` job.

Probe on a throwaway branch anyway. A probe on a PR head cannot satisfy or block
the required checks, but its skipped rows and its red `Test (probe)` stay on
that commit. The [gate-pr skill](https://github.com/sachiniyer/agent-factory/blob/master/.claude/skills/gate-pr.md)
reads every check on the head, so it will flag them until the next push moves the head.
