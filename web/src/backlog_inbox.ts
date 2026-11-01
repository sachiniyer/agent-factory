// The open question box (item panel and inbox cards) and the Inbox list
// (backlog-v2 proposal, ui.md "Inbox"). Both live across renders, so a
// backlog.changed event never eats an answer the user is typing, or its focus.

import { h } from "./dom.js";
import { icon } from "./icon.js";
import { type BacklogView, type Message, formatID } from "./backlog_model.js";
import { answerEffect, answerEffectText, inbox } from "./backlog_groups.js";
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
  answer(id: number, questionId: number, text: string): Promise<boolean>;
  select(id: number): void;
}

interface Card {
  li: HTMLElement;
  head: HTMLElement;
  box: QuestionBox;
}

/** Each open question as a card: item, age, question, handoff, what an answer
 *  does, answer box. Cards are kept by item and question. */
export class InboxList {
  readonly el = h("div", { class: "af-backlog-inbox" });
  private readonly list = h("ul", { class: "af-backlog-inbox-list" });
  private readonly empty = h("p", { class: "af-backlog-empty" }, EMPTY_INBOX);
  private cards = new Map<string, Card>();

  constructor(private readonly actions: InboxActions) {
    this.list.setAttribute("aria-label", "Inbox");
    this.el.append(this.list, this.empty);
  }

  render(view: BacklogView, sessions: readonly SessionData[]): void {
    const entries = inbox(view);
    const next = new Map<string, Card>();
    for (const { item, question } of entries) {
      const key = `${item.id}:${question.id}`;
      const card = this.cards.get(key) ?? this.card(item.id);
      const label = `${formatID(item.id)} ${item.title}`;
      const age = `Asked ${formatTime(question.at)}`;
      if (card.head.dataset.sig !== `${label}\n${age}`) {
        card.head.dataset.sig = `${label}\n${age}`;
        const open = h("button", { type: "button", class: "af-backlog-item-link" }, label);
        open.addEventListener("click", () => this.actions.select(item.id));
        card.head.replaceChildren(open, h("span", { class: "af-backlog-reason" }, age));
      }
      card.box.set(question, answerEffectText(answerEffect(item, sessions)));
      next.set(key, card);
    }
    this.cards = next;
    const want = [...next.values()].map((c) => c.li);
    const have = [...this.list.children];
    if (want.length !== have.length || want.some((li, i) => li !== have[i])) this.list.replaceChildren(...want);
    this.empty.hidden = want.length > 0;
  }

  private card(id: number): Card {
    const head = h("div", { class: "af-backlog-inbox-head" });
    const box = new QuestionBox(id, (qid, text) => this.actions.answer(id, qid, text));
    const li = h("li", { class: "af-backlog-inbox-card" }, head, box.el);
    return { li, head, box };
  }
}
