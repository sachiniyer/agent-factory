// Closes the issues a merged PR lists after a closing keyword.
//
// GitHub closes only the FIRST issue of "Fixes #a, #b, #c" (each number needs its
// own keyword) and ignores ranges like "#5975-#5980", so bundle PRs left issues
// open after merging. This reads the PR's title and body itself, then closes each
// listed issue of THIS repository as completed with one PATCH that appends
// "Closed by #<PR> (merged)" to its body — no comment, since comments cost gate
// runs. It never closes on "Refs", never touches another repo, never reopens.

const MAX_RANGE = 20;
const KEYWORD = "(?:fix|fixes|fixed|close|closes|closed|resolve|resolves|resolved)";
const ITEM = "#(\\d+)(?:[ \\t]*[-\\u2013\\u2014][ \\t]*#?(\\d+))?(?![\\w/])";
// Items are separated on ONE line only, by a comma, "and", "&" or plain spaces.
const SEPARATOR = "(?:[ \\t]*,[ \\t]*(?:and[ \\t]+)?|[ \\t]+and[ \\t]+|[ \\t]*&[ \\t]*|[ \\t]+)";

// Fenced blocks, inline code and HTML comments are prose that merely mentions a
// keyword (a PR template, a quoted example), so they never count.
function stripNonProse(text) {
  return String(text || "")
    .replace(/^[ \t]*(`{3,}|~{3,})[^\n]*\n[\s\S]*?(?:^[ \t]*\1[`~]*[ \t]*$|(?![\s\S]))/gm, "\n")
    .replace(/<!--[\s\S]*?(?:-->|$)/g, " ")
    .replace(/`[^`\n]*`/g, " ");
}

// Issue numbers listed after a closing keyword, ascending and de-duplicated.
// A range wider than MAX_RANGE (or backwards) is ignored whole.
function parseClosingRefs(...texts) {
  const found = new Set();
  const head = new RegExp(`(?<![\\w/#-])${KEYWORD}\\b:?[ \\t]*`, "gi");
  const item = new RegExp(`^${ITEM}`);
  const sep = new RegExp(`^${SEPARATOR}`);
  for (const raw of texts) {
    const text = stripNonProse(raw);
    let m;
    while ((m = head.exec(text))) {
      let rest = text.slice(head.lastIndex);
      let consumed = 0;
      for (;;) {
        const it = item.exec(rest);
        if (!it) break;
        const start = Number(it[1]);
        const end = it[2] === undefined ? start : Number(it[2]);
        if (end >= start && end - start + 1 <= MAX_RANGE) {
          for (let n = start; n <= end; n++) found.add(n);
        }
        rest = rest.slice(it[0].length);
        consumed += it[0].length;
        const s = sep.exec(rest);
        if (!s) break;
        rest = rest.slice(s[0].length);
        consumed += s[0].length;
      }
      head.lastIndex += consumed;
    }
  }
  return [...found].sort((a, b) => a - b);
}

const closedLine = (pr) => `Closed by #${pr} (merged)`;

// Closes each listed issue that is open, is an issue (not a PR) and was not
// already closed by this PR. Per-issue failures warn and never fail the caller.
async function closeFixedIssues({ github, core, owner, repo, pr, title, body }) {
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
      core.warning(`#${number}: could not close: ${error.message}`);
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
  await closeFixedIssues({ github, core, owner, repo, pr, title: process.env.PR_TITLE, body: process.env.PR_BODY });
}

module.exports = { parseClosingRefs, closeFixedIssues, run, stripNonProse, MAX_RANGE };
