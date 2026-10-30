// Proposal callouts: `> [!question]`, `> [!todo]` and `> [!decision]` blockquotes.
// Imported by both the main bundle (the open-question count) and the lazy editor
// chunk (the live-preview styling), so it must stay free of DOM and CodeMirror.

export type CalloutKind = "question" | "todo" | "decision";

export const CALLOUT_KINDS: readonly CalloutKind[] = ["question", "todo", "decision"];

export const CALLOUT_LABELS: Record<CalloutKind, string> = {
  question: "Question",
  todo: "To do",
  decision: "Decision",
};

const CALLOUT_HEAD = /^ {0,3}>[ \t]?\[!(question|todo|decision)\]/i;
const QUOTE_LINE = /^ {0,3}>/;
const FENCE = /^ {0,3}(`{3,}|~{3,})/;

/** One callout block: its kind and the character range of its lines. */
export interface Callout {
  kind: CalloutKind;
  from: number;
  to: number;
  /** The 0-based line number of the `> [!kind]` line. */
  line: number;
}

/** The callout kind that a `> [!kind]` line opens, or null for any other line. */
export function calloutKindOfLine(line: string): CalloutKind | null {
  const m = CALLOUT_HEAD.exec(line);
  return m ? (m[1]!.toLowerCase() as CalloutKind) : null;
}

/** Every callout in a markdown text, in document order. Lines inside fenced code
 *  blocks are skipped, so an example callout in a code sample does not count. A
 *  callout continues over the quote lines that follow it, and ends at the first
 *  line that is not a quote line or that opens another callout. */
export function findCallouts(text: string): Callout[] {
  const lines = text.split("\n");
  const out: Callout[] = [];
  let offset = 0;
  let fence: string | null = null;
  let open: Callout | null = null;
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]!;
    const lineEnd = offset + line.length;
    const fenceMatch = FENCE.exec(line);
    if (fence !== null) {
      if (fenceMatch && fenceMatch[1]![0] === fence[0] && fenceMatch[1]!.length >= fence.length) {
        fence = null;
      }
    } else if (fenceMatch) {
      open = null;
      fence = fenceMatch[1]!;
    } else {
      const kind = calloutKindOfLine(line);
      if (kind) {
        open = { kind, from: offset, to: lineEnd, line: i };
        out.push(open);
      } else if (open && QUOTE_LINE.test(line)) {
        open.to = lineEnd;
      } else {
        open = null;
      }
    }
    offset = lineEnd + 1;
  }
  return out;
}

export function countOpenQuestions(text: string): number {
  return findCallouts(text).filter((c) => c.kind === "question").length;
}

/** The start offset of the first question callout that begins after `pos`.
 *  With `wrap`, it falls back to the first question in the text. Null when there
 *  is none. */
export function nextQuestionOffset(text: string, pos: number, wrap = true): number | null {
  const questions = findCallouts(text).filter((c) => c.kind === "question");
  const next = questions.find((c) => c.from > pos) ?? (wrap ? questions[0] : undefined);
  return next ? next.from : null;
}
