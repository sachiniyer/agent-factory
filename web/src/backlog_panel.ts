// The Backlog view's item panel (backlog proposal, ui.md "Item panel"). The
// title field, the body section and the note box live across renders so a
// backlog.changed event never eats what the user is typing, or their focus;
// the parts above and below the body rebuild. The open question's answer box
// lives across renders for the same reason.

import { h } from "./dom.js";
import { icon } from "./icon.js";
import { BodyEditor } from "./backlog_body.js";
import { waitsOnBlock } from "./backlog_cycle.js";
import type { ItemUpdate } from "./backlog_api.js";
import {
  ACTION_LABELS, type BacklogView, type Item, type ItemAction, type MoveTarget, STATE_GLYPHS, STATE_LABELS, type Status,
  childrenOf, derivedOf, formatID, isAncestor, isParent, itemActions, itemByID, neededBy, openQuestion, safeHttpUrl,
} from "./backlog_model.js";
import { ROW_KIND_LABELS, isArchived, rowKind } from "./status.js";
import { type Usage, pct, runUse, usd } from "./backlog_usage.js";
import { SerialSaver, queueFor } from "./backlog_serial.js";
import { formatTime } from "./time.js";
import type { SessionData } from "./types.js";

function sameList(a: readonly number[], b: readonly number[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

export interface PanelActions {
  /** A promise settles true once the write and the refresh after it have landed (the body editor's acknowledgement). */
  update(id: number, update: ItemUpdate): Promise<boolean> | void;
  status(id: number, status: Status): void;
  remove(id: number): void;
  move(id: number, to: MoveTarget): void;
  /** A promise settles true once the note and the refresh after it have landed; false when it was refused. */
  note(id: number, text: string): Promise<boolean> | void;
  start(id: number): void;
  /** Sends the answer; resolves true once the daemon took it. */
  answer(id: number, questionId: number, text: string): Promise<boolean>;
  addChild(parent: number, title: string): void;
  select(id: number): void;
  close(): void;
  openSession(id: string): void;
  archiveSession(id: string): void;
  restoreSession(id: string): void;
  /** Reads sessions the rail has not loaded (archived rows are paged in lazily) into the session list. Rejects when it cannot tell. */
  loadSessions(ids: string[]): Promise<void>;
}

const ACTION_STATUS: Record<Exclude<ItemAction, "delete" | "start">, Status> = {
  queue: "queued",
  draft: "draft",
  review: "review",
  done: "done",
  drop: "dropped",
  reopen: "draft",
};

export function stateChip(view: BacklogView, id: number, reason = derivedOf(view, id).reason ?? ""): HTMLElement {
  const d = derivedOf(view, id);
  const chip = h("span", { class: `af-backlog-chip af-backlog-state-${d.state}` },
    h("span", { class: "af-backlog-glyph", ariaHidden: "true" }, STATE_GLYPHS[d.state]), STATE_LABELS[d.state]);
  return reason ? h("span", { class: "af-backlog-state" }, chip, h("span", { class: "af-backlog-reason" }, reason)) : chip;
}

function section(title: string, ...children: (Node | string)[]): HTMLElement {
  return h("section", { class: "af-backlog-section" }, h("h3", { class: "af-backlog-section-title" }, title), ...children);
}

function button(label: string, run: () => void, cls = "af-ghost"): HTMLButtonElement {
  const b = h("button", { type: "button", class: cls }, label);
  b.addEventListener("click", run);
  return b;
}

/** A one-line input that runs `submit` on Enter and clears itself. */
export function entryField(placeholder: string, label: string, submit: (text: string) => void): HTMLInputElement {
  const input = h("input", { type: "text", class: "af-backlog-entry", placeholder });
  input.setAttribute("aria-label", label);
  input.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" || e.isComposing) return;
    const text = input.value.trim();
    if (!text) return;
    e.preventDefault();
    input.value = "";
    submit(text);
  });
  return input;
}

export class ItemPanel {
  readonly el: HTMLElement;
  private readonly title: HTMLInputElement;
  private readonly body: BodyEditor;
  private readonly note: HTMLTextAreaElement;
  private readonly head: HTMLElement;
  private readonly question: HTMLElement;
  private readonly questionText = h("span", {});
  private readonly answer: HTMLTextAreaElement;
  private readonly sendAnswer: HTMLButtonElement;
  private questionId = 0;
  private readonly above: HTMLElement;
  private readonly below: HTMLElement;
  private readonly threadList: HTMLElement;
  private readonly closeButton: HTMLButtonElement;
  private pickerQuery = "";
  private pickerOpen = false;
  private moveOpen = false;
  private view: BacklogView | null = null;
  private sessions: SessionData[] = [];
  private usage: Usage | null = null;
  // Run sessions the rail did not hold, by id: asked for, answered, or unanswerable.
  private readonly lookups = new Map<string, "pending" | "done" | "failed">();
  // The title the user submitted that no confirming response has covered yet: a
  // refresh that read the old item must not put the old title back over it.
  private titleDraft: string | null = null;
  // The waits-on list the user last submitted, likewise held until acknowledged, so
  // a second change builds on it instead of on the stored list.
  private waitsDraft: number[] | null = null;
  private readonly titles: SerialSaver<string>;
  private readonly waits: SerialSaver<number[]>;

  constructor(readonly root: string, readonly id: number, private readonly actions: PanelActions) {
    this.titles = new SerialSaver((title) => actions.update(id, { title }), (title, ok) => {
      if (this.titleDraft !== title) return;   // a newer submission owns the draft
      this.titleDraft = null;
      // A refused rename gives the field back its stored title.
      if (!ok) this.repaint();
    }, queueFor(`${root}\u0000${id}\u0000title`));
    this.waits = new SerialSaver((waits_on) => actions.update(id, { waits_on }), (list, ok) => {
      if (this.waitsDraft !== null && sameList(this.waitsDraft, list)) this.waitsDraft = null;
      if (!ok) this.repaint();
    }, queueFor(`${root}\u0000${id}\u0000waits`));
    this.title = h("input", { type: "text", class: "af-backlog-title-input" });
    this.title.setAttribute("aria-label", "Title");
    const commitTitle = (): void => {
      const it = this.item();
      const t = this.title.value.trim();
      if (!it) return;
      if (t && t !== (this.titleDraft ?? it.title)) {
        this.titleDraft = t;
        this.titles.submit(t);
      } else this.title.value = this.titleDraft ?? it.title;
    };
    this.title.addEventListener("change", commitTitle);
    this.title.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.isComposing) this.title.blur();
    });
    this.body = new BodyEditor((text) => actions.update(id, { body: text }), "Body", `${root}\u0000${id}\u0000body`);
    this.note = h("textarea", { class: "af-backlog-note", rows: 2, placeholder: "Add a note…" });
    this.note.setAttribute("aria-label", "Note");
    const addNote = button("Add note", () => {
      const text = this.note.value.trim();
      if (!text) return;
      this.note.value = "";
      const done = actions.note(id, text);
      // A refused note comes back to the box, ahead of anything typed since.
      if (done) done.then((ok) => { if (!ok) this.note.value = this.note.value ? `${text}\n${this.note.value}` : text; }, () => {});
    });
    this.head = h("div", { class: "af-backlog-panel-state", tabIndex: -1 });
    this.answer = h("textarea", { class: "af-backlog-note", rows: 2, placeholder: `Answer ${formatID(id)}…` });
    this.answer.setAttribute("aria-label", "Answer");
    this.sendAnswer = button("Send answer", () => this.submitAnswer(), "af-primary");
    this.answer.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && !e.isComposing) {
        e.preventDefault();
        this.submitAnswer();
      }
    });
    this.question = h("div", { class: "af-backlog-question", role: "group", hidden: true },
      h("div", { class: "af-backlog-question-text" }, icon("circle-question"), this.questionText),
      h("div", { class: "af-backlog-note-row" }, this.answer, this.sendAnswer));
    this.question.setAttribute("aria-label", "Open question");
    this.above = h("div", { class: "af-backlog-panel-rest" });
    this.below = h("div", { class: "af-backlog-panel-rest" });
    this.threadList = h("ol", { class: "af-backlog-thread" });
    const close = h("button", { type: "button", class: "af-ghost af-backlog-close", title: "Close" }, icon("x"));
    close.setAttribute("aria-label", "Close item");
    close.addEventListener("click", () => actions.close());
    this.closeButton = close;
    const top = h("div", { class: "af-backlog-panel-top" },
      h("span", { class: "af-backlog-id" }, formatID(id)), this.title, close);
    this.el = h("aside", { class: "af-backlog-panel" }, top, this.head, this.question, this.above, section("Body", this.body.el), this.below,
      section("Thread", this.threadList, h("div", { class: "af-backlog-note-row" }, this.note, addNote)));
    this.el.setAttribute("aria-label", `Item ${formatID(id)}`);
    this.el.addEventListener("keydown", (e) => {
      if (e.key !== "Escape" || e.defaultPrevented || e.isComposing) return;
      e.preventDefault();
      actions.close();
    });
  }

  focus(): void {
    this.closeButton.focus({ preventScroll: true });
  }

  /** An absent run session is not proof it is gone: the rail pages archived rows in
   *  only when the archive filter is on. Ask the daemon for them by id once. */
  private lookUpMissing(it: Item): void {
    const missing = (it.runs ?? []).map((r) => r.session_id)
      .filter((id): id is string => !!id && !this.lookups.has(id) && !this.sessions.some((s) => s.id === id));
    if (missing.length === 0) return;
    for (const id of missing) this.lookups.set(id, "pending");
    const settle = (state: "done" | "failed"): void => {
      for (const id of missing) this.lookups.set(id, state);
      this.repaint();
    };
    this.actions.loadSessions(missing).then(() => settle("done"), () => settle("failed"));
  }

  /** The waits-on list as the user sees it: their last submission until it is acknowledged. */
  private waitsOf(it: Item): number[] {
    return this.waitsDraft ?? it.waits_on ?? [];
  }

  private setWaits(it: Item, list: number[]): void {
    this.waitsDraft = list;
    this.waits.submit(list);
    this.repaint();
  }

  /** Draws again from what the panel last rendered, for a change that arrives on its own (a lookup, a write's outcome). */
  private repaint(): void {
    if (this.view) this.render(this.view, this.sessions, this.usage);
  }

  private item(): Item | undefined {
    return this.view ? itemByID(this.view.backlog, this.id) : undefined;
  }

  private submitAnswer(): void {
    const text = this.answer.value.trim();
    const qid = this.questionId;
    if (!text || !qid || this.sendAnswer.disabled) return;
    this.sendAnswer.disabled = true;
    void this.actions.answer(this.id, qid, text).then((ok) => {
      this.sendAnswer.disabled = false;
      if (ok && this.questionId === qid && this.answer.value.trim() === text) this.answer.value = "";
    });
  }

  render(view: BacklogView, sessions: SessionData[], usage: Usage | null = null): void {
    this.view = view;
    this.sessions = sessions;
    this.usage = usage;
    const it = this.item();
    if (!it) return;
    this.lookUpMissing(it);
    if (document.activeElement !== this.title && this.titleDraft === null) this.title.value = it.title;
    this.body.setText(it.body ?? "");
    const restore = this.keepField();
    this.head.replaceChildren(stateChip(view, it.id));
    const q = openQuestion(it);
    this.question.hidden = !q;
    if ((q?.id ?? 0) !== this.questionId) this.answer.value = "";
    this.questionId = q?.id ?? 0;
    this.questionText.textContent = q ? (q.author === "af" ? `af asks: ${q.text}` : q.text) : "";
    this.above.replaceChildren(this.actionsRow(view, it));
    const below: HTMLElement[] = [this.waitsSection(view, it)];
    const children = this.childrenSection(view, it);
    if (children) below.push(children);
    below.push(this.runsSection(it));
    if (it.source?.proposal) below.push(h("p", { class: "af-backlog-source" }, icon("file-text"), `From proposal ${it.source.proposal}`));
    this.below.replaceChildren(...below);
    this.threadList.replaceChildren(...(it.thread ?? []).map((m) => h("li", { class: `af-backlog-msg af-backlog-msg-${m.kind}` },
      h("div", { class: "af-backlog-msg-meta" }, [m.kind === "note" ? "Note" : m.kind === "question" ? "Question" : "Answer",
        m.author === "user" ? "you" : m.author.startsWith("agent:") ? "agent" : m.author, formatTime(m.at),
        ...(m.closed ? [m.closed === "answered in session" ? "Answered in session" : `Closed · ${m.closed}`] : [])].join(" · ")),
      h("div", { class: "af-backlog-msg-text" }, m.text))));
    restore();
  }

  /** Keeps the focus, and a field's value, across a rebuild: the same control
   *  is found again by its tag, label and text. An item action that went away,
   *  such as Start now once the item starts, hands focus to the first action
   *  left, else to the state chip. */
  private keepField(): () => void {
    const active = document.activeElement as HTMLInputElement | null;
    const box = [this.above, this.below].find((b) => active && b.contains(active));
    if (!box || !active) return () => {};
    const tag = active.tagName;
    const label = active.getAttribute("aria-label");
    const text = active.textContent;
    const field = tag === "INPUT" || tag === "SELECT";
    const value = field ? active.value : "";
    const action = active.dataset.action;
    return () => {
      const next = action
        ? this.above.querySelector<HTMLElement>(`[data-action="${action}"]`)
        : [...box.querySelectorAll<HTMLInputElement>(tag.toLowerCase())]
          .find((el) => el.getAttribute("aria-label") === label && (field || el.textContent === text));
      if (!next) {
        if (action) (this.above.querySelector<HTMLElement>("[data-action]") ?? this.head).focus({ preventScroll: true });
        return;
      }
      if (field) (next as HTMLInputElement).value = value;
      next.focus({ preventScroll: true });
    };
  }

  private actionsRow(view: BacklogView, it: Item): HTMLElement {
    const row = h("div", { class: "af-backlog-actions" });
    for (const a of itemActions(view.backlog, it, derivedOf(view, it.id))) {
      const b = button(ACTION_LABELS[a], () => {
        if (a === "delete") this.actions.remove(it.id);
        else if (a === "start") this.actions.start(it.id);
        else this.actions.status(it.id, ACTION_STATUS[a]);
      }, a === "queue" || a === "start" ? "af-primary" : "af-ghost");
      b.dataset.action = a;
      row.append(b);
    }
    row.append(this.moveMenu(view, it));
    return row;
  }

  private moveMenu(view: BacklogView, it: Item): HTMLElement {
    const b = view.backlog;
    const others = b.items.filter((o) => o.id !== it.id && !isAncestor(b, it.id, o.id));
    const pick = (label: string, opts: Item[], to: (id: number) => MoveTarget): HTMLSelectElement => {
      const sel = h("select", { class: "af-backlog-move-select" },
        h("option", { value: "" }, label), ...opts.map((o) => h("option", { value: String(o.id) }, `${formatID(o.id)} ${o.title}`)));
      sel.setAttribute("aria-label", label);
      sel.addEventListener("change", () => {
        if (sel.value) this.actions.move(it.id, to(Number(sel.value)));
      });
      return sel;
    };
    const panel = h("div", { class: "af-backlog-move-panel" },
      button("Top", () => this.actions.move(it.id, { top: true })),
      button("Bottom", () => this.actions.move(it.id, { bottom: true })),
      pick("Above…", others, (id) => ({ before: id })),
      pick("Into…", others.filter((o) => o.id !== it.parent && o.status !== "running" && (o.runs ?? []).length === 0),
        (id) => ({ into: id })));
    const details = h("details", { class: "af-backlog-move", open: this.moveOpen }, h("summary", { class: "af-ghost" }, "Move…"), panel);
    details.addEventListener("toggle", () => { this.moveOpen = details.open; });
    return details;
  }

  private waitsSection(view: BacklogView, it: Item): HTMLElement {
    const b = view.backlog;
    const chips = h("div", { class: "af-backlog-chips" });
    for (const w of this.waitsOf(it)) {
      const target = itemByID(b, w);
      const remove = h("button", { type: "button", class: "af-backlog-chip-x", title: `Remove ${formatID(w)}` }, icon("x"));
      remove.setAttribute("aria-label", `Remove ${formatID(w)}`);
      remove.addEventListener("click", () => this.setWaits(it, this.waitsOf(it).filter((x) => x !== w)));
      const state = target ? STATE_LABELS[derivedOf(view, w).state] : "missing";
      chips.append(h("span", { class: "af-backlog-link" },
        h("span", {}, `${formatID(w)} ${target?.title ?? ""}`), h("span", { class: "af-backlog-reason" }, state), remove));
    }
    const add = button("Add", () => {
      this.pickerOpen = !this.pickerOpen;
      this.render(view, this.sessions, this.usage);
      this.el.querySelector<HTMLInputElement>(".af-backlog-picker input")?.focus();
    });
    add.classList.add("af-backlog-wait-add");
    add.setAttribute("aria-expanded", String(this.pickerOpen));
    chips.append(add);
    const parts: (Node | string)[] = [chips];
    if (this.pickerOpen) parts.push(this.picker(view, it));
    const needed = neededBy(b, it.id);
    if (needed.length > 0) {
      parts.push(h("div", { class: "af-backlog-needed" }, h("span", { class: "af-backlog-label" }, "Needed by"),
        ...needed.map((n) => this.itemLink(n))));
    }
    return section("Waits on", ...parts);
  }

  private picker(view: BacklogView, it: Item): HTMLElement {
    const b = view.backlog;
    const input = h("input", { type: "search", class: "af-backlog-entry", placeholder: "Search by id or title…", value: this.pickerQuery });
    input.setAttribute("aria-label", "Search items to wait on");
    const list = h("ul", { class: "af-backlog-picker-list" });
    const fill = (): void => {
      const q = this.pickerQuery.trim().toLowerCase().replace(/^b-/, "");
      const options = b.items.filter((o) => o.id !== it.id && !this.waitsOf(it).includes(o.id) &&
        (q === "" || String(o.id).startsWith(q) || o.title.toLowerCase().includes(q)));
      list.replaceChildren(...options.map((o) => {
        const block = waitsOnBlock(b, it.id, o.id);
        const opt = h("button", { type: "button", class: "af-backlog-option", disabled: block !== null },
          h("span", {}, `${formatID(o.id)} ${o.title}`), ...(block ? [h("span", { class: "af-backlog-reason" }, block)] : []));
        opt.addEventListener("click", () => {
          this.pickerOpen = false;
          this.pickerQuery = "";
          this.setWaits(it, [...this.waitsOf(it), o.id]);
        });
        return h("li", {}, opt);
      }));
      if (options.length === 0) list.append(h("li", { class: "af-backlog-reason" }, "No matching items"));
    };
    input.addEventListener("input", () => {
      this.pickerQuery = input.value;
      fill();
    });
    input.addEventListener("keydown", (e) => {
      if (e.key === "Escape") {
        e.preventDefault();
        this.pickerOpen = false;
        this.render(view, this.sessions, this.usage);
        this.below.querySelector<HTMLElement>(".af-backlog-wait-add")?.focus();
      }
    });
    fill();
    return h("div", { class: "af-backlog-picker" }, input, list);
  }

  private itemLink(o: Item): HTMLElement {
    const b = h("button", { type: "button", class: "af-backlog-item-link" }, `${formatID(o.id)} ${o.title}`);
    b.addEventListener("click", () => this.actions.select(o.id));
    return b;
  }

  private childrenSection(view: BacklogView, it: Item): HTMLElement | null {
    const b = view.backlog;
    const parent = isParent(b, it.id);
    if (!parent && (it.status === "running" || (it.runs ?? []).length > 0)) return null;
    const parts: (Node | string)[] = [];
    if (parent) {
      parts.push(h("ul", { class: "af-backlog-children" },
        ...childrenOf(b, it.id).map((c) => h("li", {}, this.itemLink(c), stateChip(view, c.id, "")))));
      const sw = h("input", { type: "checkbox", checked: it.sequential === true });
      sw.setAttribute("role", "switch");
      sw.addEventListener("change", () => this.actions.update(it.id, { sequential: sw.checked }));
      parts.push(h("label", { class: "af-backlog-switch" }, sw, "Run children in order"));
    }
    parts.push(entryField("Add child…", `Add a child to ${formatID(it.id)}`, (title) => this.actions.addChild(it.id, title)));
    return section("Children", ...parts);
  }

  private runsSection(it: Item): HTMLElement {
    const runs = it.runs ?? [];
    const parts: (Node | string)[] = [];
    const pr = safeHttpUrl(it.pr);
    if (pr) parts.push(h("a", { class: "af-backlog-pr", href: pr, target: "_blank", rel: "noopener noreferrer" }, icon("external-link"), "Pull request"));
    if (runs.length === 0) parts.push(h("p", { class: "af-backlog-reason" }, "No runs yet"));
    const list = h("ul", { class: "af-backlog-runs" });
    for (const run of [...runs].reverse()) {
      const s = run.session_id ? this.sessions.find((x) => x.id === run.session_id) : undefined;
      const asked = run.session_id ? this.lookups.get(run.session_id) : undefined;
      const absent = asked === "pending" ? "Checking…" : asked === "failed" ? "Unavailable" : "Gone";
      const live = s ? ROW_KIND_LABELS[rowKind(s)] : run.state === "starting" ? "Starting" : run.state === "failed" ? "Failed" : absent;
      const by = run.started_by === "runner" ? "runner" : "you";
      const meta = h("div", { class: "af-backlog-reason" }, [`Started by ${by}`, formatTime(run.started_at), live].join(" · "));
      const cost = run.session_id ? this.usage?.runs[run.session_id] : undefined;
      if (cost !== undefined && this.usage) {
        const use = runUse(run.session_id, this.usage, Date.now(), s?.account);
        const words = use === null ? [`${usd(cost)} at API prices`] : [`${pct(use)} of the 5-hour window`, `${usd(cost)} at API prices`];
        const span = h("span", { class: "af-backlog-run-use" }, words.join(" · "));
        span.title = use === null ? "No reading for the 5-hour window" : `${usd(this.usage.run_window[run.session_id] ?? 0)} in this 5-hour window`;
        meta.append(" · ", span);
      }
      const btns = h("div", { class: "af-backlog-run-actions" });
      if (s?.id) {
        const sid = s.id;
        btns.append(button("Open", () => this.actions.openSession(sid)));
        const kind = rowKind(s);
        if (kind === "archived" || kind === "lost" || kind === "dead") btns.append(button("Restore", () => this.actions.restoreSession(sid)));
        if (!isArchived(s)) btns.append(button("Archive", () => this.actions.archiveSession(sid)));
      }
      list.append(h("li", { class: "af-backlog-run" },
        h("div", { class: "af-backlog-run-main" }, h("div", { class: "af-backlog-run-title" }, run.title || run.session_id),
          meta), btns));
    }
    parts.push(list);
    return section("Runs", ...parts);
  }

  dispose(): void {
    this.body.dispose();
  }
}
