// Closes the issues a merged PR declares it fixes.
//
// GitHub closes only the FIRST issue of "Fixes #a, #b, #c" (each number needs its
// own keyword) and ignores ranges like "#5975-#5980", so bundle PRs left issues
// open after merging. This reads the PR's title and body itself, then closes each
// declared issue of THIS repository as completed with one PATCH that appends
// "Closed by #<PR> (merged)" to its body — no comment, since comments cost gate
// runs. It never closes on "Refs", never touches another repo, never reopens.
//
// THE RULE IS DELIBERATELY NARROW. A false close is worse than a miss (a sweep
// catches misses), and stripping Markdown constructs one at a time (fenced
// blocks, code spans, indented blocks, ...) kept leaving another path that
// closed an example. So this acts only on unambiguous declarations:
//   (a) the PR TITLE, a keyword anywhere in it ("fix: x (Fixes #3)"), outside
//       backtick code spans; and
//   (b) BODY lines that BEGIN at column 0 with the keyword, outside fenced
//       blocks and HTML comments.
// Indented lines (including 4-space code blocks), blockquotes, list items, inline
// code and mid-sentence mentions ("this fixes #9") never close anything.

const MAX_RANGE = 20;
const KEYWORD = "(?:fix|fixes|fixed|close|closes|closed|resolve|resolves|resolved)";
const ITEM = "#(\\d+)(?:[ \\t]*[-\\u2013\\u2014][ \\t]*#?(\\d+))?(?![\\w/])";
// No real issue number has more digits than this. Longer numbers are rejected
// before any range is expanded: past Number.MAX_SAFE_INTEGER, n + 1 rounds back
// to n, and expanding "#9007199254740992" would loop forever.
const MAX_DIGITS = 7;
// Items are separated on ONE line only, by a comma, "and", "&" or plain spaces.
const SEPARATOR = "(?:[ \\t]*,[ \\t]*(?:and[ \\t]+)?|[ \\t]+and[ \\t]+|[ \\t]*&[ \\t]*|[ \\t]+)";

// Fenced blocks, code spans and HTML comments are prose that merely mentions a
// keyword (a PR template, a quoted example), so they never count. Both follow
// CommonMark: a fence closes on a line of the same character at least as long as
// the opener (an unclosed one runs to the end of the text), and a code span is
// closed only by a backtick run of EXACTLY the opening run's length, so a
// double-backtick span around "Fixes #123" is one span, not two empty ones.
function stripFences(text) {
  const out = [];
  let fence = null;
  for (const line of text.split("\n")) {
    if (fence) {
      const close = /^ {0,3}(`{3,}|~{3,})[ \t]*$/.exec(line);
      if (close && close[1][0] === fence.char && close[1].length >= fence.length) fence = null;
      continue;
    }
    const open = /^ {0,3}(`{3,}|~{3,})(.*)$/.exec(line);
    // A backtick fence's info string may not contain a backtick (that is a span).
    if (open && !(open[1][0] === "`" && open[2].includes("`"))) {
      fence = { char: open[1][0], length: open[1].length };
      continue;
    }
    out.push(line);
  }
  return out.join("\n");
}

function stripCodeSpans(text) {
  let out = "";
  let i = 0;
  while (i < text.length) {
    if (text[i] !== "`") { out += text[i++]; continue; }
    let n = 0;
    while (text[i + n] === "`") n++;
    let j = i + n;
    let close = -1;
    while (j < text.length) {
      if (text[j] !== "`") { j++; continue; }
      let m = 0;
      while (text[j + m] === "`") m++;
      if (m === n) { close = j; break; }
      j += m;
    }
    if (close < 0) { out += text.slice(i, i + n); i += n; continue; }
    out += " ";
    i = close + n;
  }
  return out;
}

// Adds the issue numbers listed at the start of `rest` (the text right after a
// keyword) to `found`, and returns how much it consumed. A range wider than
// MAX_RANGE (or backwards) is ignored whole.
function readList(rest, found) {
  const item = new RegExp(`^${ITEM}`);
  const sep = new RegExp(`^${SEPARATOR}`);
  let consumed = 0;
  for (;;) {
    const it = item.exec(rest);
    if (!it) break;
    // An oversize number poisons the whole item (including a range that has
    // one oversize end): nothing from it is closed, and the list stops.
    if (it[1].length > MAX_DIGITS || (it[2] !== undefined && it[2].length > MAX_DIGITS)) break;
    const start = Number(it[1]);
    const end = it[2] === undefined ? start : Number(it[2]);
    if (Number.isSafeInteger(start) && Number.isSafeInteger(end) && end >= start && end - start + 1 <= MAX_RANGE) {
      for (let n = start; n <= end; n++) found.add(n);
    }
    rest = rest.slice(it[0].length);
    consumed += it[0].length;
    const s = sep.exec(rest);
    if (!s) break;
    rest = rest.slice(s[0].length);
    consumed += s[0].length;
  }
  return consumed;
}

// Issue numbers the PR declares fixed, ascending and de-duplicated (see the rule
// at the top of this file).
function parseClosingRefs(title, body) {
  const found = new Set();
  const keyword = `${KEYWORD}\\b:?[ \\t]*`;

  const titleText = stripCodeSpans(String(title || "").replace(/\r?\n/g, " "));
  const anywhere = new RegExp(`(?<![\\w/#-])${keyword}`, "gi");
  let m;
  while ((m = anywhere.exec(titleText))) {
    anywhere.lastIndex += readList(titleText.slice(anywhere.lastIndex), found);
  }

  const lineStart = new RegExp(`^${keyword}`, "i");
  const prose = stripFences(String(body || "").replace(/\r\n/g, "\n").replace(/<!--[\s\S]*?(?:-->|$)/g, " "));
  for (const line of prose.split("\n")) {
    const k = lineStart.exec(line);
    if (k) readList(line.slice(k[0].length), found);
  }
  return [...found].sort((a, b) => a - b);
}

const closedLine = (pr) => `Closed by #${pr} (merged)`;

// Closes each listed issue that is open, is an issue (not a PR) and was not
// already closed by this PR. Per-issue failures warn and never fail the caller.
//
// Only a PR merged into the repository's default branch closes anything: a
// stacked PR merges into another PR's branch, and its issues are not fixed until
// the stack lands on the default branch.
async function closeFixedIssues({ github, core, owner, repo, pr, title, body, baseRef, defaultBranch, failed = [] }) {
  if (!baseRef || !defaultBranch || baseRef !== defaultBranch) {
    core.info(`PR #${pr} merged into ${baseRef || "an unknown base"}, not ${defaultBranch || "the default branch"}: closing nothing`);
    return [];
  }
  const issues = parseClosingRefs(title, body).filter((n) => n !== pr);
  const closed = [];
  for (const number of issues) {
    try {
      const { data: issue } = await github.rest.issues.get({ owner, repo, issue_number: number });
      if (issue.pull_request) { core.info(`#${number}: is a pull request, not closing`); continue; }
      if (issue.state !== "open") { core.info(`#${number}: already ${issue.state}`); continue; }
      const line = closedLine(pr);
      if ((issue.body || "").includes(line)) continue;
      await github.rest.issues.update({
        owner, repo, issue_number: number,
        state: "closed", state_reason: "completed",
        body: issue.body ? `${issue.body}\n\n${line}` : line,
      });
      closed.push(number);
    } catch (error) {
      // A fork PR's GITHUB_TOKEN is read-only: the write is refused. That is
      // expected, so warn and carry on rather than failing the run.
      const why = error.status === 403 ? "token is read-only (fork PR?)" : error.message;
      core.warning(`#${number}: could not close: ${why}`);
      failed.push({ number, reason: why });
    }
  }
  if (closed.length) core.notice(`PR #${pr} closed ${closed.map((n) => `#${n}`).join(", ")}.`);
  return closed;
}

// Entry point for the pull_request-closed workflow; title/body come from env so
// PR text is never interpolated into a script.
async function run({ github, core, context }) {
  const pr = Number(process.env.PR_NUMBER);
  if (!Number.isInteger(pr)) throw new Error("PR_NUMBER missing");
  const { owner, repo } = context.repo;
  const failed = [];
  const closed = await closeFixedIssues({
    github, core, owner, repo, pr,
    title: process.env.PR_TITLE, body: process.env.PR_BODY,
    baseRef: process.env.PR_BASE_REF, defaultBranch: process.env.DEFAULT_BRANCH,
    failed,
  });
  // A failed close must be visible, never a green job that closed nothing.
  if (failed.length) {
    await core.summary
      .addHeading(`PR #${pr}: ${failed.length} listed issue(s) could not be closed`, 3)
      .addList(failed.map((f) => `#${f.number}: ${f.reason}`))
      .write();
    core.setFailed(`Could not close ${failed.map((f) => `#${f.number}`).join(", ")} (closed: ${closed.map((n) => `#${n}`).join(", ") || "none"}).`);
  }
}

module.exports = { parseClosingRefs, closeFixedIssues, run, stripFences, stripCodeSpans, MAX_RANGE };
