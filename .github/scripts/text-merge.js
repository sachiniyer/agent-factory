"use strict";

// Whether one file's committed merge result is nothing but its two sides' own
// changes (#4886).
//
// The update-branch proof in auto-gate.js rebuilds a merge tree path by path, and
// a path both sides changed used to end the proof outright. That was the whole
// reason a maintainer approval failed to carry across the gate's own update
// merges: #4789's `a7705950` and #4825's `52b78a17` each had exactly one such file
// (`app/home_update.go`, changed by the PR and, lines away, by master), and
// `git merge-tree --write-tree p1 p2` reproduced both committed trees exactly.
//
// The property this proves is the one `merge-tree` equality stands for: the
// committed file is the merge base with the first side's hunks and the second
// side's hunks applied, the two sets touching disjoint, non-adjacent line ranges.
// Every committed line is then either base text or a line one side wrote, so a
// merge that passes carries no authored change of its own. A hand resolution
// that picks, edits or adds anything fails, whether or not it looks plausible.
//
// This deliberately does not reimplement git's merge, and it does not claim to
// agree with git on every input. It is a line diff (Myers) per side and git's
// conflict rule: overlapping or adjacent hunks conflict, identical hunks merge
// to one copy. What it guarantees holds by construction, whatever alignment the
// diff picks: the result is the merge base with EVERY hunk of the first side and
// EVERY hunk of the second applied, and the committed bytes must equal it
// exactly. On repetitive lines the diff can align a change differently from
// git's, which makes it refuse some merges git calls clean (a lost carry, the
// safe direction) and, rarely, accept one git would have flagged. Measured
// against `git merge-file` on 12,000 random small triples of repetitive lines,
// the adversarial case for alignment: where both were clean they disagreed on
// 6 (0.05%, each a refusal here, because the committed file is git's), and this
// accepted 33 (0.28%) that git conflicted on — each still the base plus both
// sides' complete hunk sets, never a line neither side wrote, and never a merge
// `PUT update-branch` can commit, since it refuses a conflict. On the seven
// real both-changed files behind #4886 (`a7705950`, `5b6ee0ab`, `52b78a17`,
// `c0f04243`) it accepts all seven.

// Content is handled as bytes, not text (latin1 is a byte-exact round trip), and
// a NUL byte on any side means "binary": git would not attempt a text merge
// there, so neither does this.

// The edit distance past which the diff gives up. Its trace grows with the square
// of this, so the bound keeps a pathological pair from costing more than a few
// megabytes. It is per side against the merge base: the largest link on #4789
// (`web/dist/af-web.js`, 225 changed lines on the PR side) is far below it.
const MAX_EDIT_DISTANCE = 2000;

function splitLines(text) {
  return text === "" ? [] : text.split(/(?<=\n)/);
}

// The changes that turn `base` into `side`, as { start, end, lines } hunks over
// base line indices, sorted and disjoint: base lines [start, end) are replaced
// by `lines`. Null when the edit distance exceeds the bound.
function diffHunks(base, side, maxDistance = MAX_EDIT_DISTANCE) {
  let prefix = 0;
  while (prefix < base.length && prefix < side.length && base[prefix] === side[prefix]) prefix++;
  let suffix = 0;
  while (
    suffix < base.length - prefix &&
    suffix < side.length - prefix &&
    base[base.length - 1 - suffix] === side[side.length - 1 - suffix]
  ) {
    suffix++;
  }
  const a = base.slice(prefix, base.length - suffix);
  const b = side.slice(prefix, side.length - suffix);
  const n = a.length;
  const m = b.length;
  if (n === 0 && m === 0) return [];

  // Myers' O((N+M)D) greedy search. trace[d] is the furthest-reaching x per
  // diagonal k as it stood BEFORE round d, stored for k in [-d-1, d+1] only, so
  // the trace costs O(D^2) rather than O(D*(N+M)).
  const limit = Math.min(n + m, maxDistance);
  const offset = n + m + 1;
  const v = new Int32Array(2 * (n + m) + 3);
  const trace = [];
  let found = false;
  for (let d = 0; d <= limit && !found; d++) {
    trace.push(v.slice(offset - d - 1, offset + d + 2));
    for (let k = -d; k <= d; k += 2) {
      let x = k === -d || (k !== d && v[offset + k - 1] < v[offset + k + 1])
        ? v[offset + k + 1]
        : v[offset + k - 1] + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x++;
        y++;
      }
      v[offset + k] = x;
      if (x >= n && y >= m) {
        found = true;
        break;
      }
    }
  }
  if (!found) return null;

  // Walk the trace back from (n, m): each round contributes one edit, preceded
  // by its snake of equal lines. What is left at (x, x) after round 1 is the
  // opening snake of round 0, also equal.
  const ops = [];
  let x = n;
  let y = m;
  for (let d = trace.length - 1; d > 0; d--) {
    const snapshot = trace[d];
    const at = (k) => snapshot[k + d + 1];
    const k = x - y;
    const previousK = k === -d || (k !== d && at(k - 1) < at(k + 1)) ? k + 1 : k - 1;
    const previousX = at(previousK);
    const previousY = previousX - previousK;
    while (x > previousX && y > previousY) {
      ops.push("equal");
      x--;
      y--;
    }
    ops.push(x === previousX ? "insert" : "delete");
    x = previousX;
    y = previousY;
  }
  for (; x > 0; x--) ops.push("equal");
  ops.reverse();

  // Consecutive deletions and insertions are one hunk.
  const hunks = [];
  let current = null;
  let baseAt = 0;
  let sideAt = 0;
  for (const op of ops) {
    if (op === "equal") {
      if (current) hunks.push(current);
      current = null;
      baseAt++;
      sideAt++;
      continue;
    }
    if (!current) current = { start: baseAt, end: baseAt, lines: [] };
    if (op === "delete") {
      baseAt++;
      current.end = baseAt;
    } else {
      current.lines.push(b[sideAt]);
      sideAt++;
    }
  }
  if (current) hunks.push(current);
  return hunks.map((hunk) => ({ start: hunk.start + prefix, end: hunk.end + prefix, lines: hunk.lines }));
}

function sameHunk(left, right) {
  return left.start === right.start && left.end === right.end &&
    left.lines.length === right.lines.length &&
    left.lines.every((line, index) => line === right.lines[index]);
}

// The clean merge of two line arrays against their base, or null on a conflict.
// Two hunks conflict when they overlap or touch — max(start) <= min(end) — which
// includes two insertions at one point. Identical hunks on both sides merge to
// one copy, as they do in git.
function mergeLines(base, first, second, maxDistance = MAX_EDIT_DISTANCE) {
  const left = diffHunks(base, first, maxDistance);
  const right = diffHunks(base, second, maxDistance);
  if (!left || !right) return null;
  const merged = [];
  let position = 0;
  let i = 0;
  let j = 0;
  const take = (hunk) => {
    merged.push(...base.slice(position, hunk.start), ...hunk.lines);
    position = hunk.end;
  };
  while (i < left.length || j < right.length) {
    if (i >= left.length) {
      take(right[j++]);
      continue;
    }
    if (j >= right.length) {
      take(left[i++]);
      continue;
    }
    const a = left[i];
    const b = right[j];
    if (Math.max(a.start, b.start) <= Math.min(a.end, b.end)) {
      if (!sameHunk(a, b)) return null;
      take(a);
      i++;
      j++;
      continue;
    }
    if (a.start < b.start) take(left[i++]);
    else take(right[j++]);
  }
  merged.push(...base.slice(position));
  return merged;
}

// Whether `committed` is exactly the clean line merge of `first` and `second`
// against `base`. Every argument is a Buffer of the file's raw bytes.
function isCleanTextMerge({ base, first, second, committed }, maxDistance = MAX_EDIT_DISTANCE) {
  const sides = [base, first, second, committed];
  if (!sides.every(Buffer.isBuffer) || sides.some((bytes) => bytes.includes(0))) return false;
  const [baseText, firstText, secondText, committedText] = sides.map((bytes) => bytes.toString("latin1"));
  const merged = mergeLines(splitLines(baseText), splitLines(firstText), splitLines(secondText), maxDistance);
  return merged !== null && merged.join("") === committedText;
}

module.exports = { isCleanTextMerge, mergeLines, diffHunks, splitLines, MAX_EDIT_DISTANCE };
