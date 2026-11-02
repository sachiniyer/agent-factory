// The open question box (item panel and inbox cards) and the Inbox list
// (backlog-v2 proposal, ui.md "Inbox"). Both live across renders, so a
// backlog.changed event never eats an answer the user is typing, or its focus.

import { h } from "./dom.js";
import { icon } from "./icon.js";
import { type BacklogView, type Message, formatID } from "./backlog_model.js";
import { type SessionLookup, answerEffect, answerEffectText, inbox } from "./backlog_groups.js";
import { formatTime } from "./time.js";
import type { SessionData } from "./types.js";

export const EMPTY_INBOX = "No questions";

/** A question's text, the asking agent's handoff in full, what an answer
 *  does, and the answer box. */
export class QuestionBox {
  readonly el: HTMLElement;
  private readonly text = h("span", {});
  private readonly handoff = h("pre", { class: "af-backlog-handoff" });
  private readonly handoffWrap: HTMLElement;
  private readonly effect = h("p", { class: "af-backlog-reason af-backlog-effect" });
  private readonly answer: HTMLTextAreaElement;
  private readonly send: HTMLButtonElement;
  private qid = 0;

  constructor(id: number, private readonly submit: (questionId: number, text: string) => Promise<boolean>) {
    this.answer = h("textarea", { class: "af-backlog-note", rows: 2, placeholder: `Answer ${formatID(id)}…` });
    this.answer.setAttribute("aria-label", "Answer");
    this.send = h("button", { type: "button", class: "af-primary" }, "Send answer");
    this.send.addEventListener("click", () => this.sendAnswer());
    this.answer.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && !e.isComposing) {
        e.preventDefault();
        this.sendAnswer();
      }
    });
    this.handoffWrap = h("div", { class: "af-backlog-handoff-wrap", hidden: true },
      h("div", { class: "af-backlog-label" }, "Notes from the agent"), this.handoff);
    this.el = h("div", { class: "af-backlog-question", role: "group", hidden: true },
      h("div", { class: "af-backlog-question-text" }, icon("circle-question"), this.text),
      this.handoffWrap, this.effect,
      h("div", { class: "af-backlog-note-row" }, this.answer, this.send));
    this.el.setAttribute("aria-label", "Open question");
  }

  get questionId(): number {
    return this.qid;
  }

  set(q: Message | undefined, effect = ""): void {
    this.el.hidden = !q;
    if ((q?.id ?? 0) !== this.qid) this.answer.value = "";
    this.qid = q?.id ?? 0;
    this.text.textContent = q ? (q.author === "af" ? `af asks: ${q.text}` : q.text) : "";
    const notes = q?.handoff?.trim() ?? "";
    this.handoffWrap.hidden = notes === "";
    this.handoff.textContent = notes;
    this.effect.hidden = effect === "";
    this.effect.textContent = effect;
  }

  private sendAnswer(): void {
    const text = this.answer.value.trim();
    const qid = this.qid;
    if (!text || !qid || this.send.disabled) return;
    this.send.disabled = true;
    void this.submit(qid, text).then((ok) => {
      this.send.disabled = false;
      if (ok && this.qid === qid && this.answer.value.trim() === text) this.answer.value = "";
    });
  }
}

export interface InboxActions {
  /** Answers in root, the project the card was built for, never the one shown now. */
  answer(root: string, id: number, questionId: number, text: string): Promise<boolean>;
  select(id: number): void;
  /** Reads sessions the page has not loaded into the session list; rejects when it cannot tell. */
  loadSessions(ids: string[]): Promise<void>;
}

interface Card {
  li: HTMLElement;
  head: HTMLElement;
  box: QuestionBox;
}

/** Each open question as a card: item, age, question, handoff, what an answer
 *  does, answer box. Cards are kept by project, item and question: another
 *  project's item and question ids can be the same, and its card must not
 *  inherit this one's draft. */
export class InboxList {
  readonly el = h("div", { class: "af-backlog-inbox" });
  private readonly list = h("ul", { class: "af-backlog-inbox-list" });
  private readonly empty = h("p", { class: "af-backlog-empty" }, EMPTY_INBOX);
  private cards = new Map<string, Card>();
  // Run sessions the page did not hold, by id: asked for, answered, or unanswerable.
  private readonly lookups = new Map<string, SessionLookup>();
  private last: [string, BacklogView, readonly SessionData[]] | null = null;

  constructor(private readonly actions: InboxActions) {
    this.list.setAttribute("aria-label", "Inbox");
    this.el.append(this.list, this.empty);
  }

  render(root: string, view: BacklogView, sessions: readonly SessionData[]): void {
    this.last = [root, view, sessions];
    const entries = inbox(view);
    this.lookUpMissing(entries.map((e) => e.item), sessions);
    const next = new Map<string, Card>();
    for (const { item, question } of entries) {
      const key = `${root}\u0000${item.id}:${question.id}`;
      const card = this.cards.get(key) ?? this.card(root, item.id);
      const label = `${formatID(item.id)} ${item.title}`;
      const age = `Asked ${formatTime(question.at)}`;
      if (card.head.dataset.sig !== `${label}\n${age}`) {
        card.head.dataset.sig = `${label}\n${age}`;
        const open = h("button", { type: "button", class: "af-backlog-item-link" }, label);
        open.addEventListener("click", () => this.actions.select(item.id));
        card.head.replaceChildren(open, h("span", { class: "af-backlog-reason" }, age));
      }
      card.box.set(question, answerEffectText(answerEffect(item, sessions, (id) => this.lookups.get(id))));
      next.set(key, card);
    }
    this.cards = next;
    const want = [...next.values()].map((c) => c.li);
    const have = [...this.list.children];
    if (want.length !== have.length || want.some((li, i) => li !== have[i])) this.reorder(want);
    this.empty.hidden = want.length > 0;
  }

  /** An absent run session is not proof it is gone: a page holds only live sessions
   *  until it asks. Ask the daemon for each once, then draw again (Codex P2 on #6548). */
  private lookUpMissing(items: readonly { runs?: { session_id: string; state: string }[] }[], sessions: readonly SessionData[]): void {
    const missing = [...new Set(items.map((it) => it.runs?.at(-1))
      .filter((r) => r?.state === "started" && !!r.session_id).map((r) => r!.session_id))]
      .filter((id) => !this.lookups.has(id) && !sessions.some((s) => s.id === id));
    if (missing.length === 0) return;
    for (const id of missing) this.lookups.set(id, "pending");
    const settle = (state: SessionLookup): void => {
      for (const id of missing) this.lookups.set(id, state);
      if (this.last) this.render(...this.last);
    };
    this.actions.loadSessions(missing).then(() => settle("done"), () => settle("failed"));
  }

  /** Puts the cards in order. replaceChildren detaches every card, and a detached
   *  answer box loses focus and its selection: a card that survives gets both back,
   *  so a question arriving or leaving never takes the typing away (Codex P2 on #6548). */
  private reorder(want: HTMLElement[]): void {
    const active = document.activeElement as (HTMLElement & Partial<HTMLTextAreaElement>) | null;
    const keep = active && this.list.contains(active) && want.some((li) => li.contains(active)) ? active : null;
    const sel = keep && typeof keep.selectionStart === "number"
      ? { start: keep.selectionStart, end: keep.selectionEnd ?? keep.selectionStart, dir: keep.selectionDirection ?? "none" }
      : null;
    this.list.replaceChildren(...want);
    if (!keep || document.activeElement === keep) return;
    keep.focus({ preventScroll: true });
    if (sel && typeof keep.setSelectionRange === "function") keep.setSelectionRange(sel.start, sel.end, sel.dir);
  }

  private card(root: string, id: number): Card {
    const head = h("div", { class: "af-backlog-inbox-head" });
    const box = new QuestionBox(id, (qid, text) => this.actions.answer(root, id, qid, text));
    const li = h("li", { class: "af-backlog-inbox-card" }, head, box.el);
    return { li, head, box };
  }
}
