# Auto-merge (throughput mode)

`.github/workflows/auto-merge.yml` runs `.github/scripts/auto-merge.js` every 10 minutes, on manual dispatch, and after each **PR Validation** run. It squash-merges (head SHA pinned) every open PR into `master` that:

- is not a draft and has neither the `hold` nor the `no-auto-merge` label;
- is `MERGEABLE`;
- has `Build` and `Lint` check runs concluded `success` on its **current** head;
- has no unanswered **P1** Codex inline finding — a review thread whose first comment is by the Codex bot and contains `P1 Badge`, with no reply starting (after optional leading whitespace, markdown emphasis `*`/`_`, or blockquote `>` markers — a quoted `> RESOLVED` counts) with `RESOLVED`, `ACCEPTED` or `[gate-ack]`. P2/P3 are advisory — except on Detail-authored PRs (head branch starts with `detail/`), which also block on an unanswered **P2** (skip reason `unanswered Codex P2 on a Detail PR`; Detail answers its own findings, and an unanswered P2 accuracy finding left eight docs misdescribing shipped behaviour, #6006).

API economy is a hard requirement (the Actions `GITHUB_TOKEN` quota starved the old gate). Each run is two-phase GraphQL: a cheap query for all open master PRs (head, mergeable, labels, `Build`/`Lint` rollup), then one aliased query fetching review threads **only** for PRs that passed phase 1. Both log `rateLimit` cost and remaining as notices. REST is used for merges, workflow dispatches, the broken-master issue, and one `listWorkflowRuns` call to debounce. `workflow_run` triggers are debounced only when safe: a trigger is skipped only if another merger run STARTED AFTER the triggering run completed, or one is already queued — either reads state at least as new as the event. A run counts as having read state only if its "Merge ready PRs" step ran (jobs API): a run that was itself debounced ends "success" having read nothing and is never deferred to (#5786). A merely "recent" run is not enough (it read older state and would discard the signal). The schedule and manual dispatch always run.

## After a merge

`GITHUB_TOKEN` merges do not fire push workflows, so after a run that merged anything the workflow dispatches `build.yml`, `docs.yml` (with `verify_sha`), `lint.yml`, `mac-e2e.yml` (advisory: the real binary on macOS, #6384), `pr.yml` (the master-only heavy jobs since #5615), `sandbox-e2e.yml` (advisory, not a merge gate), `tui-driver-selftest.yml` and `web-selftest.yml` once on `master`. It then runs the **composition check** on the new head, in a read-only job of its own (see *Job layout* below): `go build ./...`, `go vet ./...`, `scripts/lint-file-length.sh`. On failure it opens (or comments on) one issue titled `master broken by merge batch <sha>` and refuses further merges. Each later run re-checks master and closes the issue once it passes. Fix forward on master with a PR labelled `fix-master`: see the composition halt below.

## Job layout (#7165)

The composition checks run code a PR controls: the post-merge check and the broken-master re-check run master's own scripts, a just-merged one included, and every check compiles a tree a PR wrote. So no check runs where the merger's write token or secrets are:

```
gate ──┬── recheck ──┬── land ── compose ── report
       └── fixcheck ─┘
```

- **`recheck`, `fixcheck`, `compose`** (the composition checks) each run in a job of their own with `permissions: contents: read`, no secrets, `persist-credentials: false`, and `setup-go` with `cache: false`, because `auto-release.yml` and `stable-release.yml` restore default-branch Go caches. They run no github-script. Their outputs are a verdict and nothing else: `passed` (from the check step's `outcome`, which the runner evaluates, not the checked code), the `sha` checked (`actions/checkout`'s own output, read before anything runs), and for `fixcheck` the `fixes` that passed and their `base`.
- **`gate`** (read-only), **`land`** (the write token) and **`report`** (`issues: write`) run only `auto-merge.js`, each from a fresh checkout of `github.workflow_sha`, the commit this workflow file came from, never a tree a check touched. `land` and `report` read nothing from a check but that verdict. A check that reports no verdict, because its runner was lost or it timed out, counts as failed: the issue stays open and nothing merges.
- The workflow-wide `permissions` is `{}`; each job asks for its own.

The cost is runner start-ups (a merging run has at least three jobs) and an uncached Go build per check. `.github/scripts/auto-merge-isolation.test.js` pins the layout, and runs the workflow to show that a merged PR's lint script cannot get its code run with the write token.

## Batch, then verify

Under continuous merging every master Build is cancelled by the next push (`cancel-in-progress`), so "the latest completed run" reads a stale success. The merger therefore only merges onto a master head that has been verified:

1. It reads master HEAD and queries `build.yml` and `lint.yml` runs **for that head SHA**.
2. Per workflow: any `success` → ok; else any `failure`/`timed_out` → red; else any queued/in-progress → wait; else (all cancelled, or a push that never ran it) → no live run.
3. Both ok → merge, at most `AUTO_MERGE_BATCH_SIZE` PRs (repository variable, default 8). Either red → **halt** (the job summary names the run). Either waiting → exit without merging; the merger is woken again when the run finishes (the `kick-auto-merge` step for a `workflow_dispatch`, the `workflow_run` trigger for a push). No live run → `workflow_dispatch` it on master and wait.

A PR labelled `fix-master` (Build + Lint still required on its head) is exempt from the red halt, so a red master can always be repaired through the merger.

**Test (macOS) halt.** `Test (macOS)` is the only place the darwin tests run, on master's `pr.yml`, and its run (hence `Build`) is not something `build.yml`/`lint.yml` verification sees. After a green Build/Lint verdict the merger reads the latest **completed** macOS verdict (`.github/scripts/macos-verdict.js`: master's recent `pr.yml` runs newest-first, first run whose macOS job concluded success/failure; a cancelled or still-running job is skipped). A red verdict halts exactly like a red master — `fix-master` PRs excepted — and clears when a later completed run is green. An unreadable verdict warns and proceeds. `pr.yml` no longer cancels an in-progress master run (one runs, only the newest waits), so a verdict arrives every ~15-30 minutes during active merging. The release workflows (`auto-release.yml`, `stable-release.yml`) run the same read before their preflight: a red latest verdict blocks the release; no verdict warns and proceeds (re-running the 10x-billed macOS job inside each release would triple its bill for commits it already tested).

**Closing keywords (advisory lint).** GitHub closes only the first issue after a closing keyword (`Fixes #1, #2, #3` leaves #2 and #3 open) and ignores ranges, and that holds for the merger's `GITHUB_TOKEN` merges too. The merger does **not** close issues itself: a hand-written Markdown parser holding an `issues:write` token kept closing unrelated issues (five review rounds, five constructs). Instead `closing-keyword-lint.yml` (`pull_request_target`, read-only token, checks out master, reads the PR title and body through env) warns on the PR — an annotation plus a job-summary line — when a keyword is followed by several numbers or a range, naming the fix: write `Fixes #1, fixes #2`, one keyword per number, which GitHub's own parser closes correctly. It never fails and is not a required check, so a false positive costs a warning. A manual sweep of merged PRs remains the backstop for what slips through.

**Composition halt.** An open `master broken by merge batch <sha>` issue while master still fails the composition check halts every merge but one kind (#7151): a `fix-master` PR that passes the usual selection (Build + Lint green on its head, `MERGEABLE`, no hold, no unanswered P1, and on `GITHUB_TOKEN` no workflow files) and whose **would-be merge result** passes the same composition check. Each such PR is squash-merged onto the master head in a scratch worktree (never the merger's own checkout) and checked there, so a PR whose head passes alone but whose merge onto master does not is refused. Nothing the PR controls runs in that check: `go build` and `go vet` do not execute the tree's code, the file-length lint is **master's** script restored into the tree (it lints its own parent directory, and `git checkout` never writes through a symlink the PR planted), and the job doing it holds no write token and no secrets (see *Job layout* below). The merger then lands the lowest-numbered checked fix that is still selectable at the head that was checked, one per run, after a last read confirming master has not moved; if GitHub refuses that merge it tries the next checked fix. The merge API cannot pin master, so a fix is checked before it lands and the post-merge check backs that up: a hand merge in the instant between the last read and the merge is caught there and reported on the same issue. The next run's re-check then finds master passing and closes the issue. Some fixes need a hand merge: two independent breakages (neither fix passes alone, so one PR must fix both), a fix that raises the `go` line in `go.mod` (setup-go pins master's toolchain with `GOTOOLCHAIN=local`), and a fix that must change `scripts/lint-file-length.sh` itself (master's copy judges it). `.github/scripts/auto-merge-halt-fix.test.js` runs all of this through `auto-merge.yml`'s own steps.

## UNKNOWN mergeability, and how verification re-wakes the merger

- GitHub computes `mergeable` lazily: the first read returns `UNKNOWN`. The merger re-reads only the otherwise-eligible UNKNOWN PRs, up to 3 times ~12s apart, and logs a PR that is still UNKNOWN as a skip "after 3 re-reads".
- Events made with `GITHUB_TOKEN` never start `workflow_run` workflows, so the verification runs the merger dispatches could not wake it. `build.yml` and `lint.yml` therefore end with a `kick-auto-merge` job: on a master `workflow_dispatch` run that was not cancelled (success **or** failure — a red master still has `fix-master` PRs to merge) it runs `gh workflow run auto-merge.yml --ref master`. `workflow_dispatch` is the one event `GITHUB_TOKEN` may trigger. The kicked run keeps the usual concurrency group; the debounce only applies to `workflow_run`.
- The kick job runs inside the run it announces, so that run is still `in_progress` when the kicked merger starts. An incomplete `build.yml`/`lint.yml` run is therefore classified by its non-kick jobs (jobs API): once every job except `Kick auto-merge` has concluded, the run counts as success or red, so the verdict never stays `wait` merely because the kick job has not finished.
- Why this terminates: a run dispatches verification only when it merged something (the head moved) or a verification workflow has no live run. A verified master with nothing to merge, a red master, and a master still being verified dispatch nothing; a cancelled run does not kick, so the next scheduled run recovers it.

## Workflow-touching PRs and `AUTO_MERGE_TOKEN`

`GITHUB_TOKEN` cannot merge a PR whose diff touches `.github/workflows/` (GitHub refuses: "refusing to allow a GitHub App to create or update workflow … without workflows permission"). On `GITHUB_TOKEN` the merger runs one cheap files query for the merge candidates only, skips such PRs with the reason *workflow files — needs AUTO_MERGE_TOKEN or a maintainer merge*, and lists them in the job summary instead of failing the run.

To let it merge those PRs too, the owner can add a repository secret `AUTO_MERGE_TOKEN`: a fine-grained PAT with **Contents**, **Pull requests** and **Workflows** read/write. The merge calls then use it (`secrets.AUTO_MERGE_TOKEN || github.token`), the files check is skipped, and — because a PAT merge fires push workflows itself — the post-merge dispatches are skipped to avoid duplicate runs.

## Head-branch cleanup

GitHub applies the repository's `delete_branch_on_merge` setting only to merges made with a user token, not `GITHUB_TOKEN`, so after each successful merge the merger deletes the head branch itself — best effort (a failure is logged, never fails the run). It keeps the branch when the head is a fork, is the default branch, is protected, is already gone, or when any open PR uses it as its base (deleting it would close the stacked PR); each keep is logged with its reason. Stranded branches from before this (the #4672 census) are not touched here.

## Controls

- **Hold a PR:** add the `no-auto-merge` label (`gh api -X POST repos/sachiniyer/agent-factory/issues/N/labels -f 'labels[]=no-auto-merge'`). Remove it to release.
- **Kill switch:** set the repository variable `AUTO_MERGE_ENABLED` to `false` — the workflow then does nothing. Delete it or set it to anything else to re-enable.
- **Re-enable the old gate:** set `AUTO_MERGE_ENABLED=false`, run `gh workflow enable` for `auto-gate.yml`, `auto-gate-head.yml` and `auto-gate-aggregate.yml`, and restore the ruleset's required checks to the pre-throughput set.

Selection logic is covered by `node --test .github/scripts/auto-merge*.test.js`.
