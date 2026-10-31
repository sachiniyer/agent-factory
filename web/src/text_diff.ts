// Line diffs for the Docs tab: the smallest edit that turns one buffer into
// another (so a reload keeps the cursor and scroll), and a three-way merge with
// conflict markers for "Show both".

export interface TextChange {
  from: number;
  to: number;
  insert: string;
}

// Above this many cells the LCS table costs too much; the caller then gets one
// replacement of the changed middle, which is still correct, only less precise.
const MAX_LCS_CELLS = 4_000_000;

/** For each line of `a`, the index of the matching line in `b`, or -1. The
 *  matches come from a longest common subsequence, so they only increase. */
export function matchLines(a: readonly string[], b: readonly string[]): number[] {
  const match = new Array<number>(a.length).fill(-1);
  let start = 0;
  while (start < a.length && start < b.length && a[start] === b[start]) {
    match[start] = start;
    start++;
  }
  let endA = a.length;
  let endB = b.length;
  while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) {
    endA--;
    endB--;
    match[endA] = endB;
  }
  const n = endA - start;
  const m = endB - start;
  if (n === 0 || m === 0 || n * m > MAX_LCS_CELLS) {
    return match;
  }
  const width = m + 1;
  const lcs = new Uint32Array((n + 1) * width);
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      lcs[i * width + j] = a[start + i] === b[start + j]
        ? lcs[(i + 1) * width + j + 1]! + 1
        : Math.max(lcs[(i + 1) * width + j]!, lcs[i * width + j + 1]!);
    }
  }
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[start + i] === b[start + j]) {
      match[start + i] = start + j;
      i++;
      j++;
    } else if (lcs[(i + 1) * width + j]! >= lcs[i * width + j + 1]!) {
      i++;
    } else {
      j++;
    }
  }
  return match;
}

function lineStarts(lines: readonly string[]): number[] {
  const starts = new Array<number>(lines.length + 1);
  let pos = 0;
  for (let i = 0; i < lines.length; i++) {
    starts[i] = pos;
    pos += lines[i]!.length;
  }
  starts[lines.length] = pos;
  return starts;
}

/** The changes, in `before` offsets and in document order, that turn `before`
 *  into `after`. Unchanged lines are never touched, so an editor that applies
 *  these keeps a cursor or scroll position that sits in them. */
export function diffText(before: string, after: string): TextChange[] {
  if (before === after) {
    return [];
  }
  // Each line keeps its own "\n", so the pieces join back to the exact text and
  // line offsets are document offsets.
  const a = before.split(/(?<=\n)/);
  const b = after.split(/(?<=\n)/);
  const match = matchLines(a, b);
  const aStart = lineStarts(a);
  const changes: TextChange[] = [];
  let i = 0;
  let j = 0;
  const flush = (iEnd: number, jEnd: number): void => {
    if (i !== iEnd || j !== jEnd) {
      changes.push({ from: aStart[i]!, to: aStart[iEnd]!, insert: b.slice(j, jEnd).join("") });
    }
  };
  for (let k = 0; k < a.length; k++) {
    if (match[k]! >= 0) {
      flush(k, match[k]!);
      i = k + 1;
      j = match[k]! + 1;
    }
  }
  flush(a.length, b.length);
  return changes;
}

/** Applies changes produced by diffText, for tests and for callers without an
 *  editor. */
export function applyChanges(text: string, changes: readonly TextChange[]): string {
  let out = "";
  let pos = 0;
  for (const c of changes) {
    out += text.slice(pos, c.from) + c.insert;
    pos = c.to;
  }
  return out + text.slice(pos);
}

/** Whether a save of `text`, made against `base`, has landed in `head`: every change it made is there,
 *  in order, between the lines `head` still shares with `base` on either side of it. Unlike the
 *  three-way merge this holds when another writer has edited the lines next to the save's, which the
 *  merge folds into one conflicting chunk. A save that changed nothing, or only removed lines, cannot be
 *  shown to have landed, so it is not. */
export function landedIn(base: string, text: string, head: string): boolean {
  const b = base.split("\n");
  const o = text.split("\n");
  const t = head.split("\n");
  const mo = matchLines(b, o);
  const mt = matchLines(b, t);
  let changes = 0;
  let i = 0;
  let oi = 0;
  const check = (bEnd: number, oEnd: number): boolean => {
    const inserted = o.slice(oi, oEnd);
    if (i === bEnd && inserted.length === 0) return true; // nothing between these two matches
    changes++;
    if (inserted.length === 0) return false; // a removal leaves nothing to find in the head
    let left = i - 1;
    while (left >= 0 && mt[left]! < 0) left--;
    let right = bEnd;
    while (right < b.length && mt[right]! < 0) right++;
    const window = t.slice(left >= 0 ? mt[left]! + 1 : 0, right < b.length ? mt[right]! : t.length);
    let at = 0;
    for (const line of window) if (at < inserted.length && line === inserted[at]) at++;
    return at === inserted.length;
  };
  for (let k = 0; k < b.length; k++) {
    if (mo[k]! < 0) continue;
    if (!check(k, mo[k]!)) return false;
    i = k + 1;
    oi = mo[k]! + 1;
  }
  if (!check(b.length, o.length)) return false;
  return changes > 0;
}

/** How many lines of `text` are not in `base`: the size of the edit a save of `text` made. */
export function changedLines(base: string, text: string): number {
  const o = text.split("\n");
  return matchLines(o, base.split("\n")).filter((m) => m < 0).length;
}

export const CONFLICT_MINE = "<<<<<<< mine";
export const CONFLICT_SPLIT = "=======";
export const CONFLICT_THEIRS = ">>>>>>> agent";

function sameLines(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((line, i) => line === b[i]);
}

/** A three-way line merge. Where only one side changed a region, that side wins.
 *  Where both changed it differently, both versions are kept between conflict
 *  markers, mine first. */
export function mergeWithMarkers(base: string, mine: string, theirs: string): string {
  const b = base.split("\n");
  const o = mine.split("\n");
  const t = theirs.split("\n");
  const mo = matchLines(b, o);
  const mt = matchLines(b, t);
  const out: string[] = [];
  let i = 0;
  let oi = 0;
  let ti = 0;
  for (;;) {
    while (i < b.length && mo[i] === oi && mt[i] === ti) {
      out.push(b[i]!);
      i++;
      oi++;
      ti++;
    }
    if (i >= b.length && oi >= o.length && ti >= t.length) {
      break;
    }
    let k = i;
    while (k < b.length && (mo[k]! < 0 || mt[k]! < 0)) {
      k++;
    }
    const oEnd = k < b.length ? mo[k]! : o.length;
    const tEnd = k < b.length ? mt[k]! : t.length;
    const baseChunk = b.slice(i, k);
    const mineChunk = o.slice(oi, oEnd);
    const theirChunk = t.slice(ti, tEnd);
    if (sameLines(mineChunk, baseChunk) || sameLines(mineChunk, theirChunk)) {
      out.push(...theirChunk);
    } else if (sameLines(theirChunk, baseChunk)) {
      out.push(...mineChunk);
    } else {
      out.push(CONFLICT_MINE, ...mineChunk, CONFLICT_SPLIT, ...theirChunk, CONFLICT_THEIRS);
    }
    i = k;
    oi = oEnd;
    ti = tEnd;
  }
  return out.join("\n");
}

/** Whether a buffer still holds markers that mergeWithMarkers wrote. */
export function hasConflictMarkers(text: string): boolean {
  return text.split("\n").some((line) => line === CONFLICT_MINE || line === CONFLICT_THEIRS);
}
