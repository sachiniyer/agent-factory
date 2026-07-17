"use strict";

// Auto-close stale EXTERNAL-contributor pull requests.
//
// Policy (see CLAUDE.md "External PR 12h no-response → close"): when a
// maintainer requests changes on a fork PR and the contributor then goes quiet
// for more than STALE_HOURS, close the PR with a warm, non-punitive note
// inviting them to reopen. We move too fast to keep PRs parked — but the clock
// only ever runs while the ball is in the contributor's court.
//
// Hard safety rails, in evaluation order (see decide()):
//   * only fork PRs are ever eligible — same-repo (internal) branches such as
//     our own `sachiniyer/*` session branches share the base repo's full name
//     and are NEVER touched;
//   * maintainer-authored PRs are skipped (their fork, our merge);
//   * the latest decisional maintainer review must be CHANGES_REQUESTED and the
//     contributor must not have pushed / commented / reviewed since it — if the
//     last move was the contributor's, the maintainer owes the response and we
//     keep it open;
//   * a pending maintainer APPROVAL is never closed — that's ours to merge;
//   * a change request younger than STALE_HOURS gets a grace window.
//
// The decision function `decide()` is PURE (no I/O) so the risky logic is unit
// tested in stale-external-pr.test.js. `run()` is the thin GitHub-API glue that
// gathers each PR's timeline, calls decide(), and (unless dry-run) comments,
// labels, and closes.

const STALE_HOURS = 12;
const STALE_LABEL = "auto-closed-stale";
const LABEL_COLOR = "d4c5f9";
const LABEL_DESCRIPTION =
  "Closed automatically after an external contributor went quiet on a change request";

// author_association values that mark a maintainer rather than an external
// contributor. GitHub reports these on reviews, comments, and the PR itself.
const MAINTAINER_ASSOCIATIONS = new Set(["OWNER", "MEMBER", "COLLABORATOR"]);

// Belt-and-suspenders login allowlist so a mis-reported association can never
// let us close our own automation's PRs. Mirrors auto-gate.js ALLOWED_AUTHORS.
const MAINTAINER_LOGINS = new Set([
  "sachiniyer",
  "app-detail-app",
  "app-detail-app[bot]",
]);

// Warm, non-punitive close note. The first paragraph is the verbatim message
// Sachin asked for; the footer keeps it auditable.
const CLOSE_COMMENT = [
  "Thanks for the contribution! We move fast here and don't keep PRs open long — closing this for now, but please re-open and push your adjustment whenever you're ready and we'll pick it right back up.",
  "",
  "<sub>Closed automatically by the stale-external-PR policy · reopen anytime and we'll resume · — Captain Claude 🤖</sub>",
].join("\n");

/**
 * @typedef {Object} Review
 * @property {string} author            login of the reviewer ("" if unknown)
 * @property {string} authorAssociation OWNER/MEMBER/COLLABORATOR/CONTRIBUTOR/NONE/…
 * @property {string} state             APPROVED/CHANGES_REQUESTED/COMMENTED/DISMISSED/PENDING
 * @property {?string} submittedAt      ISO timestamp
 *
 * @typedef {Object} Commit
 * @property {string} author            login of the commit author ("" if unknown)
 * @property {?string} committedAt       ISO timestamp
 *
 * @typedef {Object} Comment
 * @property {string} author            login of the commenter ("" if unknown)
 * @property {string} authorAssociation author_association of the commenter
 * @property {?string} createdAt         ISO timestamp
 *
 * @typedef {Object} PullState
 * @property {number} number
 * @property {string} state             'open' | 'closed'
 * @property {string} author            PR author login (the contributor)
 * @property {string} authorAssociation PR author's association
 * @property {string} headRepoFullName  head repo "owner/name" ("" if the fork was deleted)
 * @property {string} baseRepoFullName  base repo "owner/name"
 * @property {string} headRef           head branch name
 * @property {string[]} labels
 * @property {Review[]} reviews
 * @property {Commit[]} commits
 * @property {Comment[]} comments
 */

/**
 * Pure staleness decision for a single PR.
 *
 * @param {PullState} pr   normalized PR timeline
 * @param {Date|number} now evaluation time
 * @returns {{shouldClose: boolean, reason: string}}
 */
function decide(pr, now) {
  const nowMs = toMs(now);

  if (pr.state !== "open") {
    return skip("PR is not open");
  }

  // Fork-only. Same-repo PRs (our own sachiniyer/* session branches and any
  // other internal branch) share the base repo's full name and are never
  // eligible. A deleted fork head reports no repo but still isn't an internal
  // branch, so it counts as external.
  if (!isExternalHead(pr.headRepoFullName, pr.baseRepoFullName)) {
    return skip(
      `internal-branch PR (head ${pr.headRef || "?"} lives in the base repo)`,
    );
  }

  // Never auto-close a maintainer's own PR, even from a fork.
  if (isMaintainer(pr.author, pr.authorAssociation)) {
    return skip(`authored by maintainer ${pr.author || "(unknown)"}`);
  }

  // Latest *decisional* (APPROVED / CHANGES_REQUESTED) maintainer review.
  const decisional = (pr.reviews || [])
    .filter((r) => isMaintainer(r.author, r.authorAssociation))
    .filter((r) => r.state === "APPROVED" || r.state === "CHANGES_REQUESTED")
    .filter((r) => !Number.isNaN(toMs(r.submittedAt)))
    .sort((a, b) => toMs(a.submittedAt) - toMs(b.submittedAt));

  const latest = decisional[decisional.length - 1];
  if (!latest) {
    return skip("no maintainer changes-requested review");
  }
  if (latest.state === "APPROVED") {
    // A passing approval is ours to merge, never stale (rule 5).
    return skip("latest maintainer review is APPROVED (pending merge)");
  }

  const reviewMs = toMs(latest.submittedAt);

  // Ball-in-court: any contributor push / comment / review strictly after the
  // change request means the maintainer owes the next move — keep it open.
  const respondedMs = latestContributorResponse(pr, reviewMs);
  if (respondedMs !== null) {
    return skip(
      `contributor responded ${hoursBetween(reviewMs, respondedMs)}h after the change request (ball in maintainer's court)`,
    );
  }

  const ageHours = hoursBetween(reviewMs, nowMs);
  if (ageHours < STALE_HOURS) {
    return skip(`change request only ${ageHours}h old (< ${STALE_HOURS}h grace)`);
  }

  return {
    shouldClose: true,
    reason: `external fork PR: maintainer requested changes ${ageHours}h ago with no contributor response since`,
  };
}

/**
 * True when the PR head is a fork (or a deleted fork), false for internal
 * same-repo branches. Deleted heads report no full name and can never equal the
 * base, so they are treated as external.
 */
function isExternalHead(headRepoFullName, baseRepoFullName) {
  if (!headRepoFullName) return true;
  return headRepoFullName !== baseRepoFullName;
}

function isMaintainer(login, association) {
  if (login && MAINTAINER_LOGINS.has(login)) return true;
  return association ? MAINTAINER_ASSOCIATIONS.has(association) : false;
}

/**
 * Latest timestamp (ms) of contributor activity strictly after the review, or
 * null. Any commit newer than the review counts: a branch that advanced after a
 * change request is being worked, and biasing toward "keep open" is the safe
 * direction for a tool that closes things. Comments and reviews count only when
 * authored by the contributor — a maintainer's or third party's chatter does
 * not move the ball back to us.
 */
function latestContributorResponse(pr, reviewMs) {
  let latest = null;
  const consider = (ms) => {
    if (Number.isNaN(ms) || ms <= reviewMs) return;
    if (latest === null || ms > latest) latest = ms;
  };

  for (const c of pr.commits || []) {
    consider(toMs(c.committedAt));
  }
  for (const c of pr.comments || []) {
    if (c.author && c.author === pr.author) consider(toMs(c.createdAt));
  }
  for (const r of pr.reviews || []) {
    if (r.author && r.author === pr.author) consider(toMs(r.submittedAt));
  }
  return latest;
}

function skip(reason) {
  return { shouldClose: false, reason };
}

function toMs(value) {
  if (value === null || value === undefined) return NaN;
  const ms = value instanceof Date ? value.getTime() : Date.parse(value);
  return Number.isNaN(ms) ? NaN : ms;
}

// Whole-tenths of an hour, for readable log/reason lines.
function hoursBetween(fromMs, toMsValue) {
  return Math.round(((toMsValue - fromMs) / 36e5) * 10) / 10;
}

/**
 * Cheap pre-screen so we don't spend three paginated API calls per obviously
 * ineligible PR (closed / internal / maintainer-authored). decide() re-checks
 * all of this authoritatively with the full timeline; this only decides whether
 * fetching that timeline is worth it.
 */
function preScreen(pull, baseRepoFullName) {
  if (pull.state !== "open") {
    return { eligible: false, reason: "PR is not open" };
  }
  const headRepoFullName =
    pull.head && pull.head.repo ? pull.head.repo.full_name : "";
  if (!isExternalHead(headRepoFullName, baseRepoFullName)) {
    const ref = pull.head ? pull.head.ref : "?";
    return {
      eligible: false,
      reason: `internal-branch PR (head ${ref} lives in the base repo)`,
    };
  }
  const author = pull.user ? pull.user.login : "";
  if (isMaintainer(author, pull.author_association)) {
    return {
      eligible: false,
      reason: `authored by maintainer ${author || "(unknown)"}`,
    };
  }
  return { eligible: true, reason: "" };
}

function normalizePull(pull, baseRepoFullName, timeline) {
  return {
    number: pull.number,
    state: pull.state,
    author: pull.user ? pull.user.login : "",
    authorAssociation: pull.author_association,
    headRepoFullName:
      pull.head && pull.head.repo ? pull.head.repo.full_name : "",
    baseRepoFullName,
    headRef: pull.head ? pull.head.ref : "",
    labels: (pull.labels || []).map((l) => (typeof l === "string" ? l : l.name)),
    reviews: (timeline.reviews || []).map((r) => ({
      author: r.user ? r.user.login : "",
      authorAssociation: r.author_association,
      state: r.state,
      submittedAt: r.submitted_at,
    })),
    commits: (timeline.commits || []).map((c) => ({
      author: c.author ? c.author.login : "",
      committedAt: commitDate(c),
    })),
    comments: (timeline.comments || []).map((c) => ({
      author: c.user ? c.user.login : "",
      authorAssociation: c.author_association,
      createdAt: c.created_at,
    })),
  };
}

function commitDate(c) {
  if (!c.commit) return null;
  if (c.commit.committer && c.commit.committer.date) return c.commit.committer.date;
  if (c.commit.author && c.commit.author.date) return c.commit.author.date;
  return null;
}

/**
 * Sweep all open PRs and close the stale external ones.
 *
 * @param {Object} args
 * @param {Object} args.github  authenticated Octokit (actions/github-script)
 * @param {Object} args.context github-script context
 * @param {Object} args.core    github-script core
 * @param {boolean} args.dryRun when true, only log what would be closed
 * @returns {Promise<{evaluated:number, skipped:number, closed:number, wouldClose:number}>}
 */
async function run({ github, context, core, dryRun }) {
  const { owner, repo } = context.repo;
  const baseRepoFullName = `${owner}/${repo}`;
  const now = new Date();

  core.info(
    `stale-external-pr: sweeping ${baseRepoFullName} at ${now.toISOString()}`,
  );
  core.info(
    `stale-external-pr: mode = ${dryRun ? "DRY-RUN (nothing will be closed)" : "LIVE (stale PRs will be closed)"}`,
  );
  core.info(`stale-external-pr: threshold = ${STALE_HOURS}h; label = ${STALE_LABEL}`);

  const pulls = await github.paginate(github.rest.pulls.list, {
    owner,
    repo,
    state: "open",
    per_page: 100,
  });
  core.info(`stale-external-pr: ${pulls.length} open PR(s) to evaluate`);

  const summary = { evaluated: 0, skipped: 0, closed: 0, wouldClose: 0 };
  let labelEnsured = false;

  for (const pull of pulls) {
    summary.evaluated++;
    const tag = `#${pull.number} (${pull.user ? pull.user.login : "unknown"} · ${pull.head ? pull.head.ref : "?"}) ${pull.title || ""}`.trim();

    const pre = preScreen(pull, baseRepoFullName);
    if (!pre.eligible) {
      core.info(`stale-external-pr: SKIP ${tag} — ${pre.reason}`);
      summary.skipped++;
      continue;
    }

    let timeline;
    try {
      const [reviews, commits, issueComments, reviewComments] = await Promise.all([
        github.paginate(github.rest.pulls.listReviews, {
          owner,
          repo,
          pull_number: pull.number,
          per_page: 100,
        }),
        github.paginate(github.rest.pulls.listCommits, {
          owner,
          repo,
          pull_number: pull.number,
          per_page: 100,
        }),
        github.paginate(github.rest.issues.listComments, {
          owner,
          repo,
          issue_number: pull.number,
          per_page: 100,
        }),
        // Inline review-thread replies: a contributor can answer feedback here
        // without pushing or leaving a conversation comment. Missing it would be
        // a false-close, so it feeds the same contributor-response signal.
        github.paginate(github.rest.pulls.listReviewComments, {
          owner,
          repo,
          pull_number: pull.number,
          per_page: 100,
        }),
      ]);
      timeline = { reviews, commits, comments: issueComments.concat(reviewComments) };
    } catch (error) {
      // A fetch failure must never turn into a close. Log and skip this PR.
      core.warning(
        `stale-external-pr: SKIP ${tag} — could not load timeline: ${errText(error)}`,
      );
      summary.skipped++;
      continue;
    }

    const state = normalizePull(pull, baseRepoFullName, timeline);
    const decision = decide(state, now);
    if (!decision.shouldClose) {
      core.info(`stale-external-pr: SKIP ${tag} — ${decision.reason}`);
      summary.skipped++;
      continue;
    }

    if (dryRun) {
      core.notice(`stale-external-pr: [dry-run] WOULD close ${tag} — ${decision.reason}`);
      summary.wouldClose++;
      continue;
    }

    core.notice(`stale-external-pr: closing ${tag} — ${decision.reason}`);
    try {
      if (!labelEnsured) {
        await ensureLabel({ github, owner, repo, core });
        labelEnsured = true;
      }
      await github.rest.issues.createComment({
        owner,
        repo,
        issue_number: pull.number,
        body: CLOSE_COMMENT,
      });
      await github.rest.issues.addLabels({
        owner,
        repo,
        issue_number: pull.number,
        labels: [STALE_LABEL],
      });
      await github.rest.pulls.update({
        owner,
        repo,
        pull_number: pull.number,
        state: "closed",
      });
      summary.closed++;
    } catch (error) {
      core.warning(
        `stale-external-pr: FAILED to close ${tag}: ${errText(error)}`,
      );
    }
  }

  const tail = dryRun
    ? `${summary.wouldClose} would be closed`
    : `${summary.closed} closed`;
  core.info(
    `stale-external-pr: done — ${summary.evaluated} evaluated · ${summary.skipped} skipped · ${tail}`,
  );

  if (core.setOutput) {
    core.setOutput("evaluated", String(summary.evaluated));
    core.setOutput("skipped", String(summary.skipped));
    core.setOutput("closed", String(summary.closed));
    core.setOutput("would_close", String(summary.wouldClose));
  }

  await writeSummary({ core, baseRepoFullName, now, dryRun, summary });
  return summary;
}

async function ensureLabel({ github, owner, repo, core }) {
  try {
    await github.rest.issues.getLabel({ owner, repo, name: STALE_LABEL });
  } catch (error) {
    if (error && error.status === 404) {
      await github.rest.issues.createLabel({
        owner,
        repo,
        name: STALE_LABEL,
        color: LABEL_COLOR,
        description: LABEL_DESCRIPTION,
      });
      core.info(`stale-external-pr: created label ${STALE_LABEL}`);
    } else {
      core.warning(
        `stale-external-pr: could not verify label ${STALE_LABEL}: ${errText(error)}`,
      );
    }
  }
}

async function writeSummary({ core, baseRepoFullName, now, dryRun, summary }) {
  if (!core.summary || typeof core.summary.addHeading !== "function") return;
  const verb = dryRun ? "Would close" : "Closed";
  const count = dryRun ? summary.wouldClose : summary.closed;
  try {
    core.summary
      .addHeading("Stale external PR sweep", 3)
      .addRaw(
        `Repo \`${baseRepoFullName}\` · ${now.toISOString()} · mode: ${dryRun ? "dry-run" : "live"}`,
      )
      .addBreak()
      .addRaw(
        `Evaluated ${summary.evaluated} · skipped ${summary.skipped} · ${verb.toLowerCase()} ${count}`,
      );
    await core.summary.write();
  } catch (error) {
    core.warning(`stale-external-pr: could not write job summary: ${errText(error)}`);
  }
}

function errText(error) {
  if (!error) return "unknown error";
  return error.message ? error.message : String(error);
}

module.exports = {
  run,
  __test: {
    decide,
    preScreen,
    isExternalHead,
    isMaintainer,
    STALE_HOURS,
    STALE_LABEL,
    CLOSE_COMMENT,
  },
};
