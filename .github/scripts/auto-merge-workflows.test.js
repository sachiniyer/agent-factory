const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const { MASTER_WORKFLOWS } = require('./auto-merge.js');

// Workflows that run on a master push AND accept workflow_dispatch but are
// deliberately not dispatched after a batch. Each needs a reason.
const EXEMPT = {};

const dir = path.join(__dirname, '..', 'workflows');

// The `on:` block with comments stripped, or null.
function onBlock(text) {
  const lines = text.split('\n').map((l) => l.replace(/(^|\s)#.*$/, ''));
  const start = lines.findIndex((l) => /^["']?on["']?:/.test(l));
  if (start < 0) return null;
  const out = [lines[start]];
  for (const l of lines.slice(start + 1)) {
    if (/^\S/.test(l)) break;
    out.push(l);
  }
  return out.join('\n');
}

// A top-level `push:` trigger whose branches include master (or are unfiltered).
function pushesToMaster(on) {
  const m = on.match(/^ {2}push:\s*\n((?: {3,}.*\n?|\s*\n)*)/m);
  if (!m) return false;
  const body = m[1];
  const branches = body.match(/branches:\s*(\[[^\]]*\]|(?:\n\s*-\s*.*)+)/);
  return !branches || /\bmaster\b/.test(branches[1]);
}

const acceptsDispatch = (on) => /^ {2}workflow_dispatch:/m.test(on);

test('every workflow that runs on a master push and accepts a dispatch is dispatched after a merge batch', () => {
  const names = fs.readdirSync(dir).filter((n) => /\.ya?ml$/.test(n));
  const reachable = [];
  for (const name of names) {
    const on = onBlock(fs.readFileSync(path.join(dir, name), 'utf8'));
    assert.notEqual(on, null, `${name}: no \`on:\` block found, so this scan cannot classify it`);
    if (pushesToMaster(on) && acceptsDispatch(on)) reachable.push(name);
  }
  assert.ok(reachable.includes('sandbox-e2e.yml'), 'scan sanity: sandbox-e2e.yml should be detected');
  assert.ok(reachable.includes('build.yml'), 'scan sanity: build.yml should be detected');
  for (const name of reachable) {
    assert.ok(
      MASTER_WORKFLOWS.includes(name) || EXEMPT[name],
      `${name} runs on push to master and accepts workflow_dispatch, but a GITHUB_TOKEN merge fires no push; ` +
        'add it to MASTER_WORKFLOWS in auto-merge.js, or to EXEMPT here with a reason',
    );
  }
});

test('MASTER_WORKFLOWS names only real dispatchable workflows, and EXEMPT entries carry reasons', () => {
  for (const name of MASTER_WORKFLOWS) {
    const file = path.join(dir, name);
    assert.ok(fs.existsSync(file), `${name} in MASTER_WORKFLOWS does not exist`);
    assert.ok(acceptsDispatch(onBlock(fs.readFileSync(file, 'utf8'))), `${name} accepts no workflow_dispatch`);
  }
  for (const [name, reason] of Object.entries(EXEMPT)) {
    assert.ok(typeof reason === 'string' && reason.length > 10, `${name}: exemption needs a reason`);
  }
});

test('sandbox-e2e.yml is dispatched after a batch (#5603)', () => {
  assert.ok(MASTER_WORKFLOWS.includes('sandbox-e2e.yml'));
});
