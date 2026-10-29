// Differential test for the closing-keyword parser, with a real CommonMark parser
// as the ORACLE. The parser was patched four times for four Markdown constructs
// (fenced blocks, multi-backtick spans, indented blocks, multi-line spans), each
// found by review after it shipped. This replaces guessing the next construct:
// a generated corpus puts "Fixes #N" in every construct the rule has to treat as
// code, quote or list, and the script must close EXACTLY what the oracle sees as
// column-0 paragraph text.
//
// The oracle is mdast-util-from-markdown (micromark, CommonMark), pinned exactly
// in .github/scripts/package.json. It is a test-only dependency, installed with
// `npm ci` in the CI step that runs this file; nothing that runs the script in
// production installs it. Without it the test skips locally and FAILS under CI.

const assert = require('node:assert/strict');
const test = require('node:test');
const { parseClosingRefs } = require('./close-fixed-issues.js');

let fromMarkdown = null;
let loadError = null;
const loaded = import('mdast-util-from-markdown').then((m) => { fromMarkdown = m.fromMarkdown; }, (e) => { loadError = e; });

// The references CommonMark says sit in column-0 paragraph lines of a ROOT-level
// paragraph, outside code spans and inline HTML.
function oracle(doc) {
  const tree = fromMarkdown(doc);
  const chars = [...doc];
  const mask = (node) => {
    if (node.type === 'inlineCode' || node.type === 'html') {
      for (let i = node.position.start.offset; i < node.position.end.offset; i++) if (chars[i] !== '\n') chars[i] = ' ';
    }
    for (const child of node.children || []) mask(child);
  };
  const lines = [];
  for (const node of tree.children) if (node.type === 'paragraph') mask(node);
  const masked = chars.join('').split('\n');
  for (const node of tree.children) {
    if (node.type !== 'paragraph') continue;
    for (let l = node.position.start.line; l <= node.position.end.line; l++) lines.push(masked[l - 1]);
  }
  return parseClosingRefs('', lines.join('\n'));
}

// Each construct: [name, template]. `#N` is replaced with a unique number.
const KW = 'Fixes';
const CONSTRUCTS = [
  ['plain column-0 line', `${KW} #N`],
  ['plain, two numbers', `${KW} #N, #N`],
  ['paragraph then column-0 continuation', `intro text\n${KW} #N`],
  ['mid-sentence', `this ${KW.toLowerCase()} #N`],
  ['backtick fence', '```\n' + KW + ' #N\n```'],
  ['backtick fence with info', '```md\n' + KW + ' #N\n```'],
  ['long backtick fence', '````\n```\n' + KW + ' #N\n```\n````'],
  ['tilde fence', '~~~\n' + KW + ' #N\n~~~'],
  ['unterminated fence', '```\n' + KW + ' #N'],
  ['4-space indented', `    ${KW} #N`],
  ['tab indented', `\t${KW} #N`],
  ['2-space indented', `  ${KW} #N`],
  ['single span', `\`${KW} #N\``],
  ['double span', `\`\`${KW} #N\`\``],
  ['triple span', `\`\`\`${KW} #N\`\`\``],
  ['span mid-line', `see \`${KW} #N\` here`],
  ['multi-line span from line end', `text \`\n${KW} #N\n\``],
  ['multi-line span from col 0', `\`\n${KW} #N\n\``],
  ['multi-line double span', `\`\`\n${KW} #N\n\`\``],
  ['span closing mid-line then keyword', `\`a\nb\` ${KW} #N`],
  ['unclosed backtick', `\` ${KW} #N`],
  ['span then real declaration', `\`x\`\n${KW} #N`],
  ['html comment', `<!-- ${KW} #N -->`],
  ['multi-line html comment', `<!--\n${KW} #N\n-->`],
  ['inline html comment', `text <!-- ${KW} #N --> text`],
  ['blockquote', `> ${KW} #N`],
  ['nested blockquote', `> > ${KW} #N`],
  ['lazy blockquote continuation', `> quote\n${KW} #N`],
  ['dash list', `- ${KW} #N`],
  ['numbered list', `1. ${KW} #N`],
  ['task list', `- [ ] ${KW} #N`],
  ['list continuation indented', `- item\n  ${KW} #N`],
  ['list then column-0 line', `- item\n\n${KW} #N`],
  ['refs', `Refs #N`],
];

// Deterministic PRNG so a failure reproduces.
function rng(seed) { let s = seed >>> 0; return () => { s = (s * 1664525 + 1013904223) >>> 0; return s / 2 ** 32; }; }

function build(parts, seps) {
  let n = 100;
  let doc = '';
  parts.forEach(([, tpl], i) => {
    doc += tpl.replace(/#N/g, () => `#${n++}`);
    if (i < parts.length - 1) doc += seps[i];
  });
  return doc + '\n';
}

test('the script closes exactly what a CommonMark oracle sees as column-0 paragraph text', async (t) => {
  await loaded;
  if (!fromMarkdown) {
    if (process.env.CI) assert.fail(`the oracle (mdast-util-from-markdown) is not installed; run \`npm ci\` in .github/scripts: ${loadError}`);
    return t.skip('oracle not installed: run `npm ci` in .github/scripts');
  }
  const seps = ['\n\n', '\n'];
  const rand = rng(20261029);
  const docs = [];
  // Every construct alone, and every ordered pair with both separators…
  for (const a of CONSTRUCTS) docs.push([[a], []]);
  for (const a of CONSTRUCTS) for (const b of CONSTRUCTS) for (const sep of seps) docs.push([[a, b], [sep]]);
  // …plus a seeded random sample of triples.
  for (let i = 0; i < 1500; i++) {
    const pick = () => CONSTRUCTS[Math.floor(rand() * CONSTRUCTS.length)];
    docs.push([[pick(), pick(), pick()], [seps[Math.floor(rand() * 2)], seps[Math.floor(rand() * 2)]]]);
  }
  const failures = [];
  for (const [parts, sepList] of docs) {
    const doc = build(parts, sepList);
    const expected = oracle(doc);
    const actual = parseClosingRefs('', doc);
    // Never a false close; and no miss either, so the rule stays exactly the oracle's.
    if (JSON.stringify(actual) !== JSON.stringify(expected)) failures.push({ doc, expected, actual, parts: parts.map(([n]) => n) });
  }
  assert.equal(failures.length, 0,
    `${failures.length} of ${docs.length} documents differ from the oracle; first:\n` + JSON.stringify(failures.slice(0, 3), null, 1));
});

test('the oracle itself sees the constructs the way the rule needs (self-check against vacuous passes)', async (t) => {
  await loaded;
  if (!fromMarkdown) return t.skip('oracle not installed');
  assert.deepEqual(oracle('Fixes #1\n'), [1]);
  assert.deepEqual(oracle('    Fixes #1\n'), []);
  assert.deepEqual(oracle('> Fixes #1\n'), []);
  assert.deepEqual(oracle('text `\nFixes #1\n`\n'), []);
  assert.deepEqual(oracle('```\nFixes #1\n```\n'), []);
  assert.deepEqual(oracle('intro\nFixes #1\n'), [1]);
});
