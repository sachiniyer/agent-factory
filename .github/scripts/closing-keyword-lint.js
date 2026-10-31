// Warns when a PR's title or body puts several issue numbers (or a range) after
// ONE closing keyword. GitHub closes only the FIRST number after a keyword and
// ignores ranges, so "Fixes #1, #2, #3" leaves #2 and #3 open after the merge
// (33 issues did, once). GitHub's own parser closes every issue correctly when
// each number has its own keyword — "Fixes #1, fixes #2" — and that also works
// for the auto-merger's GITHUB_TOKEN merges. So instead of closing issues
// ourselves (a hand-written Markdown parser holding an issues:write token kept
// closing unrelated issues: five review rounds, five constructs), this makes the
// author's input unambiguous.
//
// It is ADVISORY by design: it only annotates, never fails, so a false positive
// (a code example it did not recognise) costs one warning and nothing else. It
// holds no write permission and runs no PR code.

const KEYWORD = "(?:fix|fixes|fixed|close|closes|closed|resolve|resolves|resolved)";
const ITEM = "#(\\d{1,7})(?:[ \\t]*[-\\u2013\\u2014][ \\t]*#?(\\d{1,7}))?(?![\\w/])";
const SEPARATOR = "(?:[ \\t]*,[ \\t]*(?:and[ \\t]+)?|[ \\t]+and[ \\t]+|[ \\t]*&[ \\t]*|[ \\t]+)";
const MAX_RANGE = 20;

// Code is where people quote examples; dropping it just cuts noise. Cheap and
// approximate on purpose — a missed construct is a harmless extra warning.
function stripCode(text) {
  return String(text || "")
    .replace(/\r\n/g, "\n")
    .replace(/^[ \t]*(`{3,}|~{3,})[^\n]*\n[\s\S]*?(?:^[ \t]*\1[`~]*[ \t]*$|(?![\s\S]))/gm, "\n")
    .replace(/<!--[\s\S]*?(?:-->|$)/g, " ")
    .replace(/<pre[\s\S]*?(?:<\/pre>|$)/gi, " ")
    .replace(/<code[\s\S]*?(?:<\/code>|$)/gi, " ")
    .replace(/(`+)[\s\S]*?\1/g, " ")
    .replace(/^(?: {4}|\t).*$/gm, "");
}

// Each place a keyword is followed by more than one number or by a range:
// [{ keyword, numbers: [..], range: bool, suggestion }].
function findMultiRefs(...texts) {
  const out = [];
  const head = new RegExp(`(?<![\\w/#-])(${KEYWORD})\\b:?[ \\t]*`, "gi");
  const item = new RegExp(`^${ITEM}`);
  const sep = new RegExp(`^${SEPARATOR}`);
  for (const raw of texts) {
    const text = stripCode(raw);
    let m;
    while ((m = head.exec(text))) {
      const keyword = m[1];
      let rest = text.slice(head.lastIndex);
      let consumed = 0;
      const numbers = [];
      let range = false;
      let items = 0;
      for (;;) {
        const it = item.exec(rest);
        if (!it) break;
        items++;
        const a = Number(it[1]);
        if (it[2] === undefined) numbers.push(a);
        else {
          range = true;
          const b = Number(it[2]);
          if (b >= a && b - a + 1 <= MAX_RANGE) for (let n = a; n <= b; n++) numbers.push(n);
        }
        rest = rest.slice(it[0].length);
        consumed += it[0].length;
        const s = sep.exec(rest);
        if (!s) break;
        rest = rest.slice(s[0].length);
        consumed += s[0].length;
      }
      head.lastIndex += consumed;
      if (items > 1 || range) {
        const kw = keyword.toLowerCase();
        const first = keyword;
        const suggestion = numbers.length > 1
          ? numbers.map((n, i) => `${i === 0 ? first : kw} #${n}`).join(", ")
          : `${first} #N, ${kw} #M (one keyword per issue number)`;
        out.push({ keyword, numbers, range, suggestion });
      }
    }
  }
  return out;
}

const messageFor = (f) =>
  `Write "${f.suggestion}": GitHub closes only the first number after a keyword, so the others stay open after the merge.`;

// Entry point for the workflow. Title/body come from env, never interpolated.
async function run({ core }) {
  try {
    const findings = findMultiRefs(process.env.PR_TITLE, process.env.PR_BODY);
    for (const f of findings) core.warning(messageFor(f), { title: "Closing keyword lists several issues" });
    if (findings.length) {
      await core.summary
        .addHeading("Closing keyword with several issue numbers", 3)
        .addList(findings.map(messageFor))
        .write();
    }
  } catch (error) {
    core.warning(`closing-keyword lint could not run: ${error.message}`);
  }
}

module.exports = { findMultiRefs, messageFor, run, stripCode };
