---
name: gate-pr
description: Gate and merge YOUR OWN pull request — CI, the Codex review for the exact head, zero unresolved findings, real testing, then merge
user_invocable: true
---

# Gate your own pull request

**You own your PR end to end, including the merge.** Nobody reviews it before it
reaches users. If your change breaks master, ships a regression, or has to be
reverted, that is yours to detect and fix.

Do not wait for a maintainer to gate you, and do not merge before the gates below
pass. Both failures cost the same thing: a defect in production.

## The rule every command here obeys: fail loud

A check that could not run is **not** a check that passed.

The dangerous failure mode of a merge gate is not a red gate — it is a gate that
prints something clean-looking when it never ran. `gh api` fails on auth, rate
limits, and transient 5xx. If that failure reaches `jq` through a pipe, `jq -s`
slurps an empty stream, prints `0`, and exits `0`. `0` reads as *all findings
resolved*, so a dead API call authorizes the merge it existed to block.

That is why every block below:

- starts with `set -euo pipefail`, so it is safe **run on its own** — the options
  from a previous block do not carry into a new shell;
- captures each response to a **file** and checks that request's own status,
  rather than piping `gh api` straight into `jq`;
- never uses `cmd || echo "clean"`. Both "the check failed" and "the check found
  nothing" exit non-zero, so that form cannot tell them apart;
- **validates the file before trusting it.** `> file` truncates *before* the
  command runs, so a failed fetch leaves a zero-byte file behind. That file
  parses fine and means nothing — and a `// []` fallback would quietly turn it
  into "no findings". Fetch to `.part` and `mv` into place on success, then
  check the input is a real JSON array before every gate that reads it.

Never write a gate command whose failure output is indistinguishable from its
pass output.

## Why this is not bureaucracy

On 2026-07-30 three PRs merged on green CI before their Codex reviews landed. The
reviews found real defects in all three. One was **#2705**: a forged `AF_HOME`
marker that let `af reset` classify a **foreign** session as owned, kill it, and
delete its worktree. It reached master because the merge beat the review.

CI going green is not permission to merge. The review posts minutes to hours
later, so "green" and "reviewed" are different states that look identical if you
only check the first.

## Set up

Every check below is against **the PR's head**, not your current checkout. Fetch
that state once, into `$G`, and read every gate from those files.

```bash
set -euo pipefail

PR=<n>
G="${TMPDIR:-/tmp}/gate-pr-$PR"
R=repos/sachiniyer/agent-factory
rm -rf "$G"; mkdir -p "$G"

gh api "$R/pulls/$PR"                       > "$G/pr.json.part"
gh api "$R/pulls/$PR/reviews"   --paginate  > "$G/reviews.json.part"
gh api "$R/issues/$PR/comments" --paginate  > "$G/issue-comments.json.part"
gh api "$R/pulls/$PR/comments"  --paginate  > "$G/inline.json.part"
for f in pr reviews issue-comments inline; do mv "$G/$f.json.part" "$G/$f.json"; done

HEAD=$(jq -r '.head.sha'   "$G/pr.json")
BASE=$(jq -r '.base.ref'   "$G/pr.json")
STATE=$(jq -r '.state'     "$G/pr.json")
DRAFT=$(jq -r '.draft'     "$G/pr.json")
MERGEABLE=$(jq -r '.mergeable' "$G/pr.json")

[[ "$HEAD" =~ ^[0-9a-f]{40}$ ]] || { echo "ABORT: head is not a 40-hex sha ($HEAD)"; exit 1; }

# Same .part/mv discipline — a truncated head-date is worse than a missing one,
# because an empty $HD makes the step-2 freshness compare accept any timestamp.
gh api "$R/commits/$HEAD" --jq '.commit.committer.date' > "$G/head-date.txt.part"
mv "$G/head-date.txt.part" "$G/head-date.txt"
grep -qE '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}' "$G/head-date.txt" \
  || { echo "ABORT: head-date is not an RFC3339 timestamp"; exit 1; }

[[ "$BASE" == master ]] || { echo "ABORT: base is $BASE, not master"; exit 1; }
[[ "$STATE" == open && "$DRAFT" == false ]] || { echo "ABORT: state=$STATE draft=$DRAFT"; exit 1; }
# Three states, three different actions — do not collapse them into one message.
case "$MERGEABLE" in
  true)  ;;
  false) echo "ABORT: mergeable=false — the PR conflicts with master, which stops CI entirely; rebase it"; exit 1 ;;
  *)     echo "ABORT: mergeable=$MERGEABLE — GitHub is still computing mergeability; wait and rerun, do not rebase yet"; exit 1 ;;
esac
```

`set -e` is what makes this safe: a failing `gh api` aborts before any `mv`, so
`$G` holds no half-written file for a later gate to read and call clean. It
starts from `rm -rf` for the same reason — a stale file from an earlier run is
just as wrong an answer as a truncated one.

Assign each field separately. Do **not** `eval` a composed string — a branch name
may legally contain shell metacharacters, and evaluating repository metadata as
code is an injection vector.

`BASE` must be `master`, because every comparison below is against
`origin/master` and is meaningless otherwise. A draft cannot merge.

Later blocks re-derive `$G` and `$HEAD` from `$PR`, so each one stands alone.
**Re-run this block after every push** — it overwrites `$G`, and a stale `$HEAD`
is the bug that steps 2 and 8 exist to catch.

## The gates

### 1. Every triggered check has finished, and none failed

Two different failures hide here: a triggered check that is red or still
running, and a **required context that never reported at all**. The rollup only
lists checks GitHub has for this PR, so a required context that never fired is
invisible in it — `auto-gate.js` blocks separately on `required check … is
missing`, and this gate has to as well.

```bash
set -euo pipefail
PR=<n>; G="${TMPDIR:-/tmp}/gate-pr-$PR"

HEAD=$(jq -r '.head.sha' "$G/pr.json")
R=repos/sachiniyer/agent-factory
gh api "$R/commits/$HEAD/check-runs" --paginate > "$G/checkruns.json.part"
gh api "$R/commits/$HEAD/status"                > "$G/statuses.json.part"
gh api "$R/rules/branches/master"               > "$G/rules.json.part"
for f in checkruns statuses rules; do mv "$G/$f.json.part" "$G/$f.json"; done
jq -s -e '[.[].check_runs[]] | length > 0' "$G/checkruns.json" >/dev/null \
  || { echo "ABORT: no check runs on $HEAD — the request failed, or nothing was triggered"; exit 1; }

# a) every triggered signal has finished and passed. `success` only — a skipped
#    required check does not go red, so accepting skips is how a build that
#    never ran reads as green. `Deploy` is the one conditional skip auto-gate
#    allows; CodeQL `neutral` means its analysis is still settling, not passing.
jq -s -r '[.[].check_runs[]][] | .name as $n | (.conclusion // "") as $c
  | if .status != "completed" then "  pending  \($n)"
    elif $c == "success" then empty
    elif ($c == "skipped" and $n == "Deploy" and .app.slug == "github-actions") then empty
    elif ($n == "CodeQL" and $c == "neutral") then "  settling  \($n)"
    else "  \($c)  \($n)" end' "$G/checkruns.json" > "$G/bad.txt"

jq -r '.statuses[]? | select(.state != "success") | "  \(.state)  \(.context)"' "$G/statuses.json" >> "$G/bad.txt"

# b) every required context reported FROM THE APP THE RULESET PINS IT TO, and
#    succeeded. Matching on name alone would let a same-named check from another
#    app satisfy a requirement the real one never met.
jq -s -r --slurpfile ru "$G/rules.json" --slurpfile st "$G/statuses.json" '
  [.[].check_runs[]] as $runs
  | [$ru[0][] | select(.type == "required_status_checks") | .parameters.required_status_checks[]]
  | .[]
  | . as $req
  | ($runs | map(select(.name == $req.context
        and (($req.integration_id // null) == null or (.app.id == $req.integration_id))))) as $mine
  | ($st[0].statuses // [] | map(select(.context == $req.context and ($req.integration_id // null) == null))) as $sctx
  | if (($mine | length) == 0 and ($sctx | length) == 0)
    then "  MISSING REQUIRED  \($req.context)\(if $req.integration_id then " (app \($req.integration_id))" else "" end)"
    elif (($mine | map(select(.status == "completed"
            and (.conclusion == "success"
                 or (.conclusion == "skipped" and $req.context == "Deploy" and .app.slug == "github-actions"))))
           | length) == 0
          and ($sctx | map(select(.state == "success")) | length) == 0)
    then "  REQUIRED NOT GREEN  \($req.context)"
    else empty end' "$G/checkruns.json" >> "$G/bad.txt"

[[ -s "$G/bad.txt" ]] && { cat "$G/bad.txt"; echo "not every check has passed — do not merge"; exit 1; }
echo "all checks passed, all required contexts reported"
```

Wait for **every** signal, not just required ones. Some workflows (for example
`web-selftest`) are deliberately not required, so "no pending required check"
will happily let you merge while a real suite is still running or already red.
CodeQL reports mid-run non-success states; only a completed conclusion is real —
which is why an incomplete `CheckRun` counts as pending rather than passing.

**A skipped required check is not a passed one.** `.github/workflows/pr.yml`
documents the trap directly: a failed dependency leaves `Build` SKIPPED, and a
skipped required check never goes red, so GitHub's own merge button stays live.
`auto-gate.js` blocks it by demanding `conclusion === "success"`, allowing a
conditional skip only for `Deploy` — this gate mirrors that exactly. Treating
any skip as a pass is how a build that never ran gets merged.

**Match a required check by its app, not just its name.** The ruleset pins each
required context to an `integration_id` (here `15368`, GitHub Actions), and
`auto-gate.js` enforces it — `checkRunMatchesSpec` compares `run.app.id` against
the spec, and its test suite covers a spoof `Build` from app `999` sitting
alongside the real one still in progress. Name-only matching would let that
spoof satisfy a requirement the real check never met.

This is why the commands above read `commits/$HEAD/check-runs` rather than
`gh pr view --json statusCheckRollup`: **the rollup does not expose the app at
all.** Its `CheckRun` carries only `__typename`, `completedAt`, `conclusion`,
`detailsUrl`, `name`, `startedAt`, `status`, and `workflowName` — so an
app-aware check is not expressible against it. The REST endpoint is also the one
`auto-gate.js` itself uses, which is the point: the hand gate should read the
same source as the automated one.

Do not parse the human table from `gh pr checks` — it is presentation output
whose columns can change under you. `gh pr checks --json` and `gh api --slurp`
are **not** available in the gh 2.45 installed here, though newer builds have
them; `gh api` and `jq -s` work on both.

### 2. A Codex review exists **for this exact head**

The artifact carries `Reviewed commit: <sha>` and lands in **either** reviews or
issue comments. Extract that SHA and compare it — counting artifacts is not
enough, because a review of an older commit still matches a naive text search.

This selects the single newest artifact whose reviewed SHA prefixes `$HEAD`, the
same way `evaluateCodex` does in `.github/scripts/auto-gate.js`. Everything
downstream reads **that one verdict**, so a stale artifact can neither satisfy a
gate nor block one.

```bash
set -euo pipefail
PR=<n>; G="${TMPDIR:-/tmp}/gate-pr-$PR"
for f in pr reviews issue-comments; do
  jq -s -e 'length > 0' "$G/$f.json" >/dev/null || { echo "ABORT: $G/$f.json is missing or empty — rerun Set up"; exit 1; }
done
HEAD=$(jq -r '.head.sha' "$G/pr.json")

jq -s --arg head "$HEAD" '
  add
  | map(select(.user.login == "chatgpt-codex-connector[bot]"))
  | map(select((.body // "") | test("\\bCodex Review\\b"; "i")))
  | map(select((.body // "") | test("reached your Codex usage limits for code reviews"; "i") | not))
  | map(select((.body // "")
      | capture("(?:\\*\\*Reviewed commit:\\*\\*|Reviewed commit:)\\s*`(?<sha>[0-9a-f]{7,40})`"; "i")
      | .sha | ascii_downcase as $rc
      | ($head | ascii_downcase | startswith($rc))))
  | sort_by(.updated_at // .submitted_at // .created_at // "")
  | last // empty
' "$G/reviews.json" "$G/issue-comments.json" > "$G/verdict.json"

# A marker from an earlier run must never outlive it: once a real verdict exists
# for this head, the freshness and body checks below have to run again.
rm -f "$G/verdict-override"

if [[ ! -s "$G/verdict.json" ]]; then
  # The ONLY sanctioned way past a missing verdict. It must name the exact head,
  # so it cannot be set once and forgotten across pushes. See "When no review
  # exists" — this is an override, and it is loud on purpose.
  if [[ -n "${GATE_SUBSTITUTE_REVIEW:-}" ]] && [[ "$GATE_SUBSTITUTE_REVIEW" == *"$HEAD"* ]]; then
    echo "OVERRIDE: no Codex verdict for $HEAD"
    echo "  substitute review: $GATE_SUBSTITUTE_REVIEW"
    echo "  bypassed: step 3 (verdict body). Steps 1, 4-8 still apply and are NOT bypassed."
    echo "  record this in the PR body before merging, naming the reviewer and this SHA"
    : > "$G/verdict-override"
  else
    echo "NOT REVIEWED at $HEAD — this is not 'clean'"
    echo "  to merge on a recorded substitute review, see 'When no review exists';"
    echo "  it requires GATE_SUBSTITUTE_REVIEW to name $HEAD"
    exit 1
  fi
fi

if [[ ! -e "$G/verdict-override" ]]; then
  # A verdict older than the commit it claims to review did not review it.
  # LC_ALL=C so the timestamp compare is bytewise, not locale collation.
  VAT=$(jq -r '.updated_at // .submitted_at // .created_at' "$G/verdict.json")
  HD=$(cat "$G/head-date.txt")
  [[ -n "$HD" ]] || { echo "ABORT: empty head-date — an empty bound accepts any verdict; rerun Set up"; exit 1; }
  LC_ALL=C; [[ "$VAT" > "$HD" ]] || { echo "ABORT: verdict $VAT predates head commit $HD"; exit 1; }

  jq -r --arg head "$HEAD" '"verdict for \($head) at \(.updated_at // .submitted_at // .created_at)"' "$G/verdict.json"
fi
```

An empty verdict means **not reviewed yet** — not "clean". A usage-limit comment
is also not a review; the filter drops it exactly as `parseReviewedCommit` does.
Trigger a review with a `@codex review` comment and wait. If none arrives, read
**"When no review exists"** below — do not skip to step 8.

### 3. No `P0`–`P3` finding in the verdict body

Codex sometimes puts a finding in the verdict body without a live inline comment.
An inline count of zero does not mean clean.

Scan **only the exact-head verdict** from step 2. Scanning every Codex body
instead is its own bug in the other direction: bodies are immutable, so a
P1 in a review of an older commit would block the PR forever even after the
current head reviews clean.

```bash
set -euo pipefail
PR=<n>; G="${TMPDIR:-/tmp}/gate-pr-$PR"

if [[ -e "$G/verdict-override" ]]; then
  echo "SKIPPED: no Codex verdict body to scan — running on the substitute review recorded in step 2"
  exit 0
fi
[[ -s "$G/verdict.json" ]] || { echo "ABORT: no exact-head verdict — rerun step 2; an absent verdict is not clean"; exit 1; }
jq -r '.body // ""' "$G/verdict.json" > "$G/verdict-body.txt"
[[ -s "$G/verdict-body.txt" ]] || { echo "ABORT: verdict has an empty body — rerun step 2"; exit 1; }

if grep -nEi '\bP[0-3]\b' "$G/verdict-body.txt"; then
  echo "BODY FINDING in the exact-head verdict — do not merge"; exit 1
fi
echo "verdict body clean"
```

**Do not** write this as `grep … || echo "clean"`. A failed read and a
no-match grep both exit non-zero, so that form reports clean when the check did
not run. An unrun check is not a passed check.

### 4. Zero unresolved inline findings

This mirrors `.github/scripts/auto-gate.js`. A reply only resolves a finding when
it comes from an **allowed author** and carries a whole-word `RESOLVED` or
`ACCEPTED` — note `UNRESOLVED` contains `RESOLVED` as a substring, so match on
word boundaries.

```bash
set -euo pipefail
PR=<n>; G="${TMPDIR:-/tmp}/gate-pr-$PR"
jq -s -e 'length > 0 and all(type == "array")' "$G/inline.json" >/dev/null \
  || { echo "ABORT: $G/inline.json is missing, empty, or not a JSON array — rerun Set up"; exit 1; }

jq -s '
  add as $all
  | ["sachiniyer","app-detail-app","app-detail-app[bot]"] as $allowed
  | ($all
     | map(select(
         .in_reply_to_id != null
         and (.user.login | IN($allowed[]))
         and ((((.body // "") | test("\\b(RESOLVED|ACCEPTED)\\b"))
               or ((.body // "") | contains("[gate-ack]"))))))
     | map(.in_reply_to_id)) as $resolved
  | $all
  | map(select(
      .user.login == "chatgpt-codex-connector[bot]"
      and .in_reply_to_id == null
      and .line != null
      and ((.id | IN($resolved[])) | not)))
  | map({id, path, line})
' "$G/inline.json" > "$G/unresolved.json"

jq -r '.[] | "  \(.id)  \(.path):\(.line)"' "$G/unresolved.json"
N=$(jq 'length' "$G/unresolved.json")
[[ "$N" == 0 ]] || { echo "$N unresolved finding(s) — do not merge"; exit 1; }
echo "no unresolved inline findings"

# A RESOLVED reply is a CLAIM. The commit is the evidence. If the head predates
# the finding the reply claims to fix, that fix cannot be in what you are about
# to merge. ACCEPTED is exempt — it claims the finding does not apply, which
# needs no commit.
HD=$(cat "$G/head-date.txt")
[[ -n "$HD" ]] || { echo "ABORT: empty head-date — rerun Set up"; exit 1; }

jq -s -r --arg hd "$HD" '
  add as $all
  | ["sachiniyer","app-detail-app","app-detail-app[bot]"] as $allowed
  | ($all
     | map(select(
         .in_reply_to_id != null
         and (.user.login | IN($allowed[]))
         and ((.body // "") | test("\\bRESOLVED\\b"))))
     | map(.in_reply_to_id)) as $claimed
  | $all
  | map(select(
      .user.login == "chatgpt-codex-connector[bot]"
      and .in_reply_to_id == null
      and .line != null
      and (.id | IN($claimed[]))
      and (.created_at >= $hd)))
  | .[] | "  \(.id)  \(.path):\(.line)  filed \(.created_at), head committed \($hd)"
' "$G/inline.json" > "$G/unpushed.txt"

[[ -s "$G/unpushed.txt" ]] && {
  cat "$G/unpushed.txt"
  echo "RESOLVED was claimed but the head is older than the finding — the fix is not pushed"
  exit 1
}
echo "every RESOLVED finding predates the head commit"
```

**A resolution marker is a claim, not evidence.** Replying `RESOLVED` clears the
finding for the gate whether or not you actually pushed the fix, and that is how
#2799 shipped broken: its findings were cleared in-thread, Auto Gate merged the
moment the count hit zero, and the fix commit landed minutes *after* the merge.
Master then carried the uncorrected commands — the exact defects the review had
caught (#2822).

The check above is the cheapest necessary condition: a fix for a finding filed
at time T cannot exist in a commit made before T. It does not prove the fix is
*correct*, only that something was pushed after the finding. This is one place
the hand gate is deliberately **stricter** than `auto-gate.js`, which trusts the
marker alone — stricter is fine, looser never is.

Read from the captured file rather than piping `gh api … --jq` into `jq`, and
check that file first. Two reasons, and the first is the one that bites:

- A pipe hides the failure. `gh api` dying leaves `jq -s` an empty stream, which
  prints `0` — indistinguishable from a genuinely clean PR. An empty *file* does
  the same thing, which is why the guard above runs before the query and why
  the filter says `add` rather than `add // []`.
- `gh api --paginate --jq` runs the filter **per page**, so a finding on page 1
  whose resolving reply is on page 2 is counted unresolved. Without `--jq`,
  `--paginate` merges the pages into one array, and `jq -s 'add'` flattens
  whatever shape arrives — after the guard has established there is something
  to flatten.

Do not reach for `gh api --slurp` — it is **not in the gh 2.45 installed here**
and fails with `unknown flag: --slurp`, even though newer gh builds do have it.
A command that errors out is not a check, and `jq -s` works on every version.

**Inline findings override the verdict, never the other way round.** A PR can
carry a "didn't find any major issues" verdict for the exact head *and* live
findings from an earlier pass on that same head. Step 3 coming back clean does
not excuse this step.

Clear each finding by replying **to that comment id, under this PR**:

```bash
gh api "repos/sachiniyer/agent-factory/pulls/$PR/comments/<comment-id>/replies" \
  -f body='RESOLVED — <what you actually changed>'
```

- `RESOLVED` — you fixed it. Say what you did; a bare marker tells a later reader nothing.
- `ACCEPTED` — it does not apply. Say why. Never ignore a finding silently.
- A **top-level PR comment does not clear a finding.** It must be a reply.
- A fix without a reply still blocks. Fix *and* reply.

### 5. If you pushed after the review, the review is stale

**Evidence has a head SHA too.** Re-run **Set up** and step 2 after every push —
that is the whole point of comparing the SHA rather than counting artifacts.
Re-trigger `@codex review`, and re-run any play-test or manual verification
against the new head.

### 6. You actually tested it

Test `$HEAD` in a **clean, isolated worktree**. Your development checkout is not
a substitute: `gh pr checkout` keeps staged, unstaged, and untracked files when
they do not conflict, so a scenario script or fixture that exists only on your
disk can turn the suite green for code that is not in the commit you are about
to merge.

```bash
set -euo pipefail
PR=<n>; G="${TMPDIR:-/tmp}/gate-pr-$PR"
HEAD=$(jq -r '.head.sha' "$G/pr.json")

git fetch --no-tags origin "refs/pull/$PR/head"
FETCHED=$(git rev-parse FETCH_HEAD)
[[ "$FETCHED" == "$HEAD" ]] || { echo "ABORT: refs/pull/$PR/head is $FETCHED, not $HEAD — someone pushed; restart"; exit 1; }

WT="$G-worktree"   # a sibling of $G, so re-running Set up cannot delete it out from under git
git worktree remove --force "$WT" 2>/dev/null || true
git worktree add --detach "$WT" "$HEAD"
cd "$WT"
[[ "$(git rev-parse HEAD)" == "$HEAD" ]] || { echo "ABORT: worktree is not at $HEAD"; exit 1; }
[[ -z "$(git status --porcelain)" ]] || { git status --porcelain; echo "ABORT: worktree is dirty"; exit 1; }
```

Run every gate and every manual test from `$WT`. Re-check `git status
--porcelain` after the run: a suite that leaves the tree dirty was reading or
writing files the commit does not contain. Tear it down with `git worktree
remove --force "$WT"` when you are done — not with `rm -rf`, which leaves the
worktree registered and blocks the next run's `git worktree add`.

- **Cheap checks locally, the matrix in CI.** Run `gofmt -l .`, `go build ./...`, `golangci-lint run --timeout=3m --fast`, `deadcode -test ./...`, `scripts/lint-file-length.sh`, and `go test` on **only the package you changed**. Do **not** run `make test-container`, `make remote-roundtrip-container`, or `make playtest-container` as a routine gate — CI runs `go test -race ./...` on every push, so a local container run duplicates it while rebuilding the whole Go tree; ~20 sessions doing that concurrently took the shared box to a load average of 160. Push, then fix what CI reports on your PR head. One targeted container run is fine to reproduce a CI failure you cannot diagnose from the logs — then stop.
- **Never bare `go test ./...` on the host. If your change is in `daemon/` or `app/`, run no tests for it locally at all — push and let CI test it.** Not `go test ./daemon/`, not a `-run`-scoped subset, not `-race`. This is a safety rule rather than a performance one: those tests spawn real `af` daemons and drive real tmux on a box where the maintainer's own daemon and ~15 live sessions are running, so a local run risks killing production sessions, not just burning CPU. `go test $(go list ./... | grep -vE '/(daemon|app)')` if you need breadth elsewhere.
- **Watch every new test FAIL first**, against the unmodified tree. Quote the failure in the PR. A test never observed red is not evidence.
- **TUI changes: drive the real TUI.** `app/` tests swap the backend factory and cannot see real provisioning — a green unit suite proves nothing about them. The scenario subcommand needs a script path:
  ```bash
  scripts/testbox.sh scenario scripts/tui-<issue>-scenario.sh
  ```
  See `scripts/tui-2599-scenario.sh` for the shape.
- **A PR-specific scenario does not replace the shared acceptance gate.** Your scenario proves your change works; `make tui-driver-selftest` proves you did not break someone else's. Run both. Require the self-test to report **all** steps green — match the `N/N` in its final `SELF-TEST PASSED` line rather than a hard-coded number, because the suite grows.

  These two are the **only** container runs that survive the no-routine-containers rule above, and only for a TUI-touching PR. They are not exempt because TUI work is special — they are exempt because **CI does not run them at all.** `go test -race ./...` covers the Go suite, but nothing in `.github/workflows/` invokes `scripts/testbox.sh selftest`, so skipping them locally means the interaction is never exercised anywhere. That is also why `auto-gate.js` demands the `play-tested` label on TUI paths. If your PR touches no TUI path, run neither.
- **tmux work: isolated socket only.** `-L <unique-name>`, removed afterward. Never the default server, never `tmux kill-server`, never `af reset`. An agent once destroyed every live session on this box that way (#2175).
- **A fake must model production's real error shape**, not the shape your assertion needs. #2711 shipped a test that injected `context.DeadlineExceeded` directly while production returned `signal: killed` — green, and proving a property the code did not have.

### 7. Branch shape

Compare the **PR head** against master, not your current checkout:

```bash
set -euo pipefail
PR=<n>; G="${TMPDIR:-/tmp}/gate-pr-$PR"
HEAD=$(jq -r '.head.sha' "$G/pr.json")

git fetch --no-tags origin master "refs/pull/$PR/head"
git diff --stat "origin/master...$HEAD"
git log --oneline "origin/master..$HEAD"
```

Only the files your change needs. Unrelated files mean the branch is stacked or
stale — fix it rather than merging it.

For a decomposition PR the diff must show exactly the one source file being
decomposed, its split files, and any expected `scripts/file-length-allowlist.txt`
or docs changes. Anything else is a tangled branch.

### 8. Merge

```bash
gh pr merge "$PR" --squash --match-head-commit "$HEAD"
```

`--match-head-commit` pins the merge to the head you actually gated. Without it,
a push landing after `$HEAD` was captured — or during the gates — merges an
unvetted commit, which is the same staleness bug as step 2 in the other
direction.

**Never `--auto`.** `gh pr merge --auto` is GitHub-native auto-merge: it merges
as soon as the *branch ruleset's* required checks pass, which on `master` is
`Lint` and `Build` only. It does not consult `.github/scripts/auto-gate.js`, so
arming it routes around steps 2–4 entirely — and it fires while the Codex review
is still minutes-to-hours away, so the finding count it merges on is zero
because nothing has looked yet. Either merge by hand once every gate above
holds, or leave the PR alone and let the Auto Gate workflow merge it. (Detail
dead-code PRs from `app/detail-app` are the exception the repo already allows to
auto-merge on clean gates.)

Do not use another merge strategy unless the maintainer explicitly asks.

Your PR body needs `Closes #<issue>`. Since #2745 the Auto Gate token has
`issues: write`, so a linked issue closes on merge — verify it did, and close it
explicitly if not.

## When no review exists

Codex reviewing is intermittent: the account hits usage limits, and stretches
pass where PRs get no verdict at all. Absence shows up two ways, and **neither
is a pass**:

- **No artifact.** Step 2 writes an empty `verdict.json`. That means the review
  step *did not run*, not that it ran and found nothing.
- **A usage-limit comment** (`reached your Codex usage limits for code reviews`).
  Step 2 filters it out, and so does `parseReviewedCommit` in `auto-gate.js`.
  A message saying the reviewer declined to look is the opposite of a clean
  verdict, and it is the one most likely to be misread as "Codex responded".

Auto Gate blocks an unreviewed head — `Codex has not reviewed head <sha> yet` —
so it will not merge for you. What to do:

1. **Re-trigger** with a `@codex review` comment and wait. Limits reset.
2. **If it still does not post, get a real review from something that is not
   you.** Dispatch a reviewer af session, or run `/code-review`. Self-review is
   the state this gate exists to prevent, and re-reading your own diff does not
   change that.
3. **Record it on the PR**, naming the reviewer and the SHA it covers — the
   substitute needs a head SHA for the same reason the Codex artifact does.
4. **Say so in the PR body**: "Codex did not review `<sha>`; reviewed instead by
   `<who>`." Merging an unreviewed head is an override. Overrides are allowed;
   silent ones are not, because the next person reading the PR cannot tell an
   unreviewed merge from a reviewed one.
5. **Then run the gates with the override set**, which is the only sanctioned
   way past step 2:

   ```bash
   export GATE_SUBSTITUTE_REVIEW="reviewed by <who> at $HEAD — <where it is recorded>"
   ```

   It must contain the exact head SHA, so it cannot be exported once and
   forgotten across pushes — a new head invalidates it exactly as it invalidates
   a Codex verdict. Step 2 prints a loud `OVERRIDE:` line naming what is
   bypassed, and **only step 3 (the verdict body) is bypassed** — there is no
   verdict body to scan. Steps 1 and 4–8 still run and still block: a substitute
   review does not excuse a red check, and live inline findings from an earlier
   Codex pass on this same head remain live.

Without this variable the gates hard-fail, which is deliberate — the fallback
has to be an explicit, recorded act, not a step you can drift into by skipping a
command that returned non-zero.

Never write "no findings" when the truth is "nobody looked". Those are different
claims, and collapsing them is the exact failure this gate exists to prevent —
in prose this time instead of in a shell pipeline.

## After you merge

Watch master. If the build goes red, if your change appears in the daemon log, or
if a **late** Codex finding lands on your already-merged PR — that is yours to
handle. Check back on your own work rather than assuming it landed clean.

## Reviewing someone else's PR

Same gates, and set `PR` to theirs — every command above is parameterised on
`$PR`, and step 6 builds its own worktree, so nothing reads your local checkout.
Additionally: verify the claims rather than reading the summary. The
highest-value review comments this repo has produced came from checking whether a
stated mechanism was real — several PR descriptions have been confidently wrong
about the cause while the fix happened to be right.
