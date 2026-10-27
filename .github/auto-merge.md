# Auto-merge (throughput mode)

`.github/workflows/auto-merge.yml` runs `.github/scripts/auto-merge.js` every 10 minutes, on manual dispatch, and after each **PR Validation** run. It squash-merges (head SHA pinned) every open PR into `master` that:

- is not a draft and has neither the `hold` nor the `no-auto-merge` label;
- is `MERGEABLE`;
- has `Build` and `Lint` check runs concluded `success` on its **current** head;
- has no unanswered **P1** Codex inline finding — a review thread whose first comment is by the Codex bot and contains `P1 Badge`, with no reply containing `RESOLVED`, `ACCEPTED` or `[gate-ack]`. P2/P3 are advisory.

API economy is a hard requirement (the Actions `GITHUB_TOKEN` quota starved the old gate). Each run is two-phase GraphQL: a cheap query for all open master PRs (head, mergeable, labels, `Build`/`Lint` rollup), then one aliased query fetching review threads **only** for PRs that passed phase 1. Both log `rateLimit` cost and remaining as notices. REST is used for merges, workflow dispatches, the broken-master issue, and one `listWorkflowRuns` call to debounce. `workflow_run` triggers are debounced: a run is skipped if another auto-merge run started within the last 3 minutes (the 10-minute schedule and manual dispatch always run).

## After a merge

`GITHUB_TOKEN` merges do not fire push workflows, so after a run that merged anything the workflow dispatches `build.yml`, `docs.yml` (with `verify_sha`), `lint.yml`, `pr.yml` (the master-only heavy jobs since #5615), `tui-driver-selftest.yml` and `web-selftest.yml` once on `master`. It then runs the **composition check** on the new head — `go build ./...`, `go vet ./...`, `scripts/lint-file-length.sh`. On failure it opens (or comments on) one issue titled `master broken by merge batch <sha>` and refuses further merges. Each later run re-checks master and closes the issue once it passes. Fix forward on master.

## Batch, then verify

Under continuous merging every master Build is cancelled by the next push (`cancel-in-progress`), so "the latest completed run" reads a stale success. The merger therefore only merges onto a master head that has been verified:

1. It reads master HEAD and queries `build.yml` and `lint.yml` runs **for that head SHA**.
2. Per workflow: any `success` → ok; else any `failure`/`timed_out` → red; else any queued/in-progress → wait; else (all cancelled, or a push that never ran it) → no live run.
3. Both ok → merge, at most `AUTO_MERGE_BATCH_SIZE` PRs (repository variable, default 8). Either red → **halt** (the job summary names the run). Either waiting → exit without merging; the `workflow_run` trigger on Build/Lint wakes the merger again when it finishes. No live run → `workflow_dispatch` it on master and wait.

A PR labelled `fix-master` (Build + Lint still required on its head) is exempt from the red halt, so a red master can always be repaired through the merger.

Other halt: an open `master broken by merge batch <sha>` issue while master still fails the composition check.

## Workflow-touching PRs and `AUTO_MERGE_TOKEN`

`GITHUB_TOKEN` cannot merge a PR whose diff touches `.github/workflows/` (GitHub refuses: "refusing to allow a GitHub App to create or update workflow … without workflows permission"). On `GITHUB_TOKEN` the merger runs one cheap files query for the merge candidates only, skips such PRs with the reason *workflow files — needs AUTO_MERGE_TOKEN or a maintainer merge*, and lists them in the job summary instead of failing the run.

To let it merge those PRs too, the owner can add a repository secret `AUTO_MERGE_TOKEN`: a fine-grained PAT with **Contents**, **Pull requests** and **Workflows** read/write. The merge calls then use it (`secrets.AUTO_MERGE_TOKEN || github.token`), the files check is skipped, and — because a PAT merge fires push workflows itself — the post-merge dispatches are skipped to avoid duplicate runs.

## Controls

- **Hold a PR:** add the `no-auto-merge` label (`gh api -X POST repos/sachiniyer/agent-factory/issues/N/labels -f 'labels[]=no-auto-merge'`). Remove it to release.
- **Kill switch:** set the repository variable `AUTO_MERGE_ENABLED` to `false` — the workflow then does nothing. Delete it or set it to anything else to re-enable.
- **Re-enable the old gate:** set `AUTO_MERGE_ENABLED=false`, run `gh workflow enable` for `auto-gate.yml`, `auto-gate-head.yml` and `auto-gate-aggregate.yml`, and restore the ruleset's required checks to the pre-throughput set.

Selection logic is covered by `node --test .github/scripts/auto-merge.test.js`.
