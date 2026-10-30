// The Docs tab (proposal mode): a doc strip, a header with the push chip, the
// save status and the open-question count, a conflict banner, and the editor. This
// module is the entry of the lazy bundle dist/lazy/docs-editor.js (see build.mjs);
// docs_tab.ts in the main bundle loads it the first time a Docs tab opens. What
// it imports from the main bundle's modules gets a second copy in this bundle, so
// keep those imports small.

import { errorText } from "./api.js";
import { countOpenQuestions } from "./callouts.js";
import { createDocsEditor, type DocsEditor } from "./docs_editor.js";
import {
  changeAction,
  nextQuestionDoc,
  orderDocs,
  questionCountLabel,
  statusText,
} from "./docs_model.js";
import { PushChip, type PushView } from "./docs_push.js";
import { DocSync, type Draft, type SyncStatus } from "./docs_sync.js";
import { h } from "./dom.js";
import { icon } from "./icon.js";
import {
  addDoc,
  changeNamesDoc,
  DOC_NAME_PATTERN,
  getDoc,
  getProposal,
  holdPush,
  type ProposalChange,
  type ProposalDocRef,
  type ProposalInfo,
  putDoc,
  type PushStatus,
  removeDoc,
  sendChanges,
} from "./proposal_api.js";
import { pressDistance, TAB_PRESS_LIMITS, tabPressVerdict } from "./tabtouch.js";
import { diffText } from "./text_diff.js";

// The doc each proposal last showed, so a rebuilt pane opens where it was.
const lastDoc = new Map<string, string>();

// Tabs that closed with a save still in flight, per doc. A tab that reopens the
// doc waits for it, so its first load does not return text older than what the
// user just typed.
const closing = new Map<string, Set<DocSync>>();

function draftKey(sessionId: string, slug: string, doc: string): string {
  return `af.docs.draft\n${sessionId}\n${slug}\n${doc}`;
}

/** Per-viewer storage for an unresolved conflict draft. A draft never goes into
 *  the proposal doc; it comes back as a conflict when the doc opens again. */
function draftStore(key: string): NonNullable<ConstructorParameters<typeof DocSync>[0]["draft"]> {
  return {
    load: (): Draft | null => {
      try {
        const raw = window.localStorage.getItem(key);
        return raw === null ? null : (JSON.parse(raw) as Draft);
      } catch {
        return null;
      }
    },
    store: (draft: Draft): void => {
      try {
        window.localStorage.setItem(key, JSON.stringify(draft));
      } catch {
        // Storage can be unavailable; the draft then lives only in memory.
      }
    },
    clear: (): void => {
      try {
        window.localStorage.removeItem(key);
      } catch {
        // Nothing stored, nothing to clear.
      }
    },
  };
}

const REFRESH_DEBOUNCE_MS = 300;

export interface DocsPaneOptions {
  sessionId: string;
  slug: string;
  token: string;
  /** Registers for proposal.changed events; returns the unsubscribe. */
  subscribe(fn: (change: ProposalChange) => void): () => void;
  /** Registers for proposal.push events; returns the unsubscribe. */
  subscribePush(fn: (status: PushStatus) => void): () => void;
  /** Registers for event-stream reconnects; returns the unsubscribe. */
  subscribeResync(fn: () => void): () => void;
}

export class DocsPane {
  private readonly root: HTMLElement;
  private readonly strip: HTMLElement;
  private readonly addButton: HTMLButtonElement;
  private readonly menu: HTMLElement;
  private readonly docMenu: HTMLElement;
  private docMenuTab: HTMLElement | null = null;
  private longPress: { timer: number; x: number; y: number; pointerId: number } | null = null;
  private readonly status: HTMLElement;
  private readonly statusText: HTMLElement;
  private readonly questions: HTMLButtonElement;
  private readonly banner: HTMLElement;
  private readonly body: HTMLElement;
  private readonly editorHost: HTMLElement;
  private readonly notice: HTMLElement;
  private readonly push: HTMLElement;
  private readonly chipText: HTMLElement;
  private readonly chipSep: HTMLElement;
  private readonly chipAction: HTMLButtonElement;
  private readonly chip: PushChip;
  /** Counts push statuses applied, so a GetProposal reply older than one is not applied. */
  private pushSeq = 0;

  private info: ProposalInfo | null = null;
  private ordered: ProposalDocRef[] = [];
  private current: ProposalDocRef | null = null;
  /** The sync of the doc being shown; it is also in `syncs`. */
  private sync: DocSync | null = null;
  /** One sync per doc this pane opened. A doc that is not shown keeps its state,
   *  an unresolved conflict draft included. */
  private readonly syncs = new Map<string, DocSync>();
  private editor: DocsEditor | null = null;
  private freshBuffer = true;
  private readonly texts = new Map<string, string>();
  private generation = 0;
  private refreshTimer: number | null = null;
  private disposed = false;
  private readonly unsubscribe: () => void;
  private readonly unsubscribePush: () => void;
  private readonly unsubscribeResync: () => void;
  private readonly onPageHide = (): void => {
    for (const s of this.syncs.values()) s.saveBeforeLeaving();
  };
  private readonly onDocMouseDown = (e: MouseEvent): void => {
    if (!this.menu.hidden && !this.menu.contains(e.target as Node) && e.target !== this.addButton &&
      !this.addButton.contains(e.target as Node)) {
      this.closeMenu();
    }
    if (!this.docMenu.hidden && !this.docMenu.contains(e.target as Node)) {
      this.closeDocMenu(false);
    }
  };

  constructor(host: HTMLElement, private readonly opts: DocsPaneOptions) {
    this.strip = h("div", { class: "af-docs-strip", role: "tablist" });
    this.strip.setAttribute("aria-label", "Proposal docs");
    this.addButton = h("button", { type: "button", class: "af-ghost af-docs-add", title: "Add doc" }, icon("plus"));
    this.addButton.setAttribute("aria-label", "Add doc");
    this.addButton.setAttribute("aria-haspopup", "menu");
    this.addButton.setAttribute("aria-expanded", "false");
    this.addButton.addEventListener("click", () => (this.menu.hidden ? this.openMenu() : this.closeMenu()));
    this.menu = h("div", { class: "af-docs-menu", role: "menu", hidden: true });
    this.menu.addEventListener("keydown", (e) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        this.closeMenu();
        this.addButton.focus();
      }
    });
    const addWrap = h("div", { class: "af-docs-addwrap" }, this.addButton, this.menu);
    this.docMenu = h("div", { class: "af-docs-menu af-docs-docmenu", role: "menu", hidden: true });
    this.docMenu.addEventListener("keydown", (e) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        this.closeDocMenu(true);
      }
    });
    const stripWrap = h("div", { class: "af-docs-stripwrap" }, this.strip, addWrap, this.docMenu);
    this.statusText = h("span", { class: "af-docs-status-text" });
    this.status = h("span", { class: "af-docs-status", role: "status" }, icon("loader-circle"), this.statusText);
    this.questions = h("button", { type: "button", class: "af-ghost af-docs-questions", hidden: true });
    this.questions.addEventListener("click", () => void this.goToNextQuestion());
    this.chipText = h("span", { class: "af-docs-chip-text" });
    this.chipSep = h("span", { class: "af-docs-sep", ariaHidden: "true", hidden: true }, " · ");
    this.chipAction = h("button", { type: "button", class: "af-ghost af-docs-chip-action", hidden: true });
    this.chipAction.addEventListener("click", () => this.runChipAction());
    const chip = h("span", { class: "af-docs-chip" }, this.chipText, this.chipSep, this.chipAction);
    this.push = h("div", { class: "af-docs-slot af-docs-push", hidden: true }, chip);
    this.chip = new PushChip({
      now: () => performance.now(),
      setTimer: (fn, ms) => window.setTimeout(fn, ms),
      clearTimer: (handle) => window.clearTimeout(handle as number),
      render: (view) => this.renderPush(view),
    });
    const tools = h("div", { class: "af-docs-tools" }, this.push, this.status, this.questions);
    const head = h("div", { class: "af-docs-head" }, stripWrap, tools);
    this.banner = h("div", { class: "af-docs-banner", role: "alert", hidden: true });
    this.editorHost = h("div", { class: "af-docs-editor" });
    this.notice = h("div", { class: "af-docs-notice", hidden: true });
    this.body = h("div", { class: "af-docs-body" }, this.notice, this.editorHost);
    this.root = h("div", { class: "af-docs" }, head, this.banner, this.body);
    this.root.dataset.proposal = opts.slug;
    host.replaceChildren(this.root);
    this.unsubscribe = opts.subscribe((change) => this.handleChange(change));
    this.unsubscribePush = opts.subscribePush((status) => {
      if (this.disposed || status.sessionId !== opts.sessionId || status.slug !== opts.slug) return;
      this.applyPush(status);
    });
    this.unsubscribeResync = opts.subscribeResync(() => this.resync());
    document.addEventListener("mousedown", this.onDocMouseDown, true);
    window.addEventListener("pagehide", this.onPageHide);
    this.setStatus("loading");
    void this.loadProposal();
  }

  dispose(): void {
    this.disposed = true;
    this.unsubscribe();
    this.unsubscribePush();
    this.unsubscribeResync();
    this.chip.dispose();
    document.removeEventListener("mousedown", this.onDocMouseDown, true);
    window.removeEventListener("pagehide", this.onPageHide);
    this.cancelLongPress();
    if (this.refreshTimer !== null) window.clearTimeout(this.refreshTimer);
    for (const name of [...this.syncs.keys()]) this.closeSync(name, false);
    this.sync = null;
    this.editor?.destroy();
    this.editor = null;
  }

  /** Closes the doc's sync: its unsaved edits are sent, and a conflict draft stays
   *  in storage unless `forget` says the doc is gone. */
  private closeSync(name: string, forget: boolean): void {
    const sync = this.syncs.get(name);
    if (!sync) return;
    this.syncs.delete(name);
    if (this.sync === sync) this.sync = null;
    const key = draftKey(this.opts.sessionId, this.opts.slug, name);
    sync.dispose();
    if (forget) draftStore(key).clear();
    const set = closing.get(key) ?? new Set<DocSync>();
    set.add(sync);
    closing.set(key, set);
    void sync.whenQuiet().then(() => {
      set.delete(sync);
      if (set.size === 0 && closing.get(key) === set) closing.delete(key);
    });
  }

  /** The doc is being deleted: every sync of it, the shown one and a tab that closed
   *  earlier and may still be retrying, stops. Their unsaved edits stay stored until
   *  the delete lands (`finishDelete`) or fails. */
  private suspendDoc(name: string): DocSync[] {
    const key = draftKey(this.opts.sessionId, this.opts.slug, name);
    const syncs = [this.syncs.get(name), ...(closing.get(key) ?? [])].filter((s): s is DocSync => s !== undefined);
    for (const s of syncs) s.suspend();
    this.syncs.delete(name);
    if (this.sync && syncs.includes(this.sync)) this.sync = null;
    return syncs;
  }

  /** The delete landed or failed: the suspended syncs end. A failed delete keeps the
   *  stored draft for the tab that reopens the doc. */
  private finishDelete(name: string, suspended: DocSync[], deleted: boolean): void {
    const key = draftKey(this.opts.sessionId, this.opts.slug, name);
    for (const s of suspended) s.abandon(!deleted);
    closing.delete(key);
    if (deleted) draftStore(key).clear();
  }

  /** The event stream reconnected: events may have been missed, so every open
   *  doc and the doc list are refreshed. Unsaved edits are kept and merge on save. */
  resync(): void {
    if (this.disposed) return;
    for (const sync of this.syncs.values()) sync.reconnect();
    this.scheduleListRefresh();
    for (const doc of this.ordered) {
      if (!this.syncs.has(doc.name)) void this.refreshText(doc);
    }
  }

  private memoryKey(): string {
    return `${this.opts.sessionId}\n${this.opts.slug}`;
  }

  private showNotice(message: string, retry: (() => void) | null): void {
    const children: (Node | string)[] = [h("p", { class: "af-docs-notice-text" }, message)];
    if (retry) {
      const btn = h("button", { type: "button", class: "af-ghost" }, "Retry");
      btn.addEventListener("click", retry);
      children.push(btn);
    }
    this.notice.replaceChildren(...children);
    this.notice.hidden = false;
    this.editorHost.hidden = true;
  }

  private hideNotice(): void {
    this.notice.hidden = true;
    this.editorHost.hidden = false;
  }

  private async loadProposal(): Promise<void> {
    const gen = ++this.generation;
    const seq = this.pushSeq;
    let info: ProposalInfo;
    try {
      info = await getProposal(this.opts.sessionId, this.opts.slug, this.opts.token);
    } catch (e) {
      if (this.disposed || gen !== this.generation) return;
      this.setStatus("error");
      this.showNotice(`Could not load proposal “${this.opts.slug}”: ${errorText(e)}`, () => void this.loadProposal());
      return;
    }
    if (this.disposed || gen !== this.generation) return;
    this.applyInfo(info, seq);
    const remembered = lastDoc.get(this.memoryKey());
    const first = this.ordered.find((d) => d.name === remembered) ?? this.ordered[0] ?? null;
    if (!first) {
      this.setStatus("saved");
      this.showNotice("This proposal has no docs yet. Add one with +.", null);
      return;
    }
    void this.openDoc(first);
    for (const doc of this.ordered) {
      if (doc.name !== first.name) void this.refreshText(doc);
    }
  }

  /** `pushSeq` is this.pushSeq when the GetProposal request went out. */
  private applyInfo(info: ProposalInfo, pushSeq: number): void {
    if (pushSeq === this.pushSeq) this.applyPush(info.push);
    this.info = info;
    this.ordered = orderDocs(info.docs);
    if (this.current) {
      this.current = this.ordered.find((d) => d.name === this.current!.name) ?? this.current;
    }
    this.renderStrip();
    this.renderQuestions();
  }

  private renderStrip(): void {
    const tabs = this.ordered.map((doc) => {
      const selected = doc.name === this.current?.name;
      const tab = h("button", { type: "button", class: "af-docs-tab", role: "tab" }, doc.name);
      tab.setAttribute("aria-selected", selected ? "true" : "false");
      tab.tabIndex = selected ? 0 : -1;
      tab.title = doc.file;
      tab.addEventListener("click", () => void this.openDoc(doc));
      tab.addEventListener("keydown", (e) => this.onStripKey(e, doc));
      tab.addEventListener("contextmenu", (e) => {
        e.preventDefault();
        this.cancelLongPress();
        const at = e.button === -1 || (e.clientX === 0 && e.clientY === 0) ? null : { x: e.clientX, y: e.clientY };
        this.openDocMenu(doc, tab, at);
      });
      tab.addEventListener("pointerdown", (e) => this.startLongPress(e, doc, tab));
      tab.addEventListener("pointermove", (e) => {
        const p = this.longPress;
        if (p && p.pointerId === e.pointerId && tabPressVerdict({ heldMs: 0,
          movedPx: pressDistance(p.x, p.y, e.clientX, e.clientY) }, TAB_PRESS_LIMITS) === "abandon") {
          this.cancelLongPress();
        }
      });
      for (const type of ["pointerup", "pointercancel", "pointerleave"] as const) {
        tab.addEventListener(type, () => this.cancelLongPress());
      }
      return tab;
    });
    this.strip.replaceChildren(...tabs);
  }

  // iOS Safari fires no contextmenu event on a long press, so touch opens the
  // menu from its own timer.
  private startLongPress(e: PointerEvent, doc: ProposalDocRef, tab: HTMLElement): void {
    if (e.pointerType !== "touch") return;
    this.cancelLongPress();
    const { clientX: x, clientY: y } = e;
    const timer = window.setTimeout(() => {
      this.longPress = null;
      this.openDocMenu(doc, tab, { x, y });
    }, TAB_PRESS_LIMITS.holdMs);
    this.longPress = { timer, x, y, pointerId: e.pointerId };
  }

  private cancelLongPress(): void {
    if (this.longPress) window.clearTimeout(this.longPress.timer);
    this.longPress = null;
  }

  private openDocMenu(doc: ProposalDocRef, tab: HTMLElement, at: { x: number; y: number } | null): void {
    this.closeMenu();
    const remove = h("button", { type: "button", class: "af-docs-menuitem", role: "menuitem" }, "Delete");
    remove.addEventListener("click", () => this.deleteDoc(doc, remove));
    this.docMenu.replaceChildren(remove);
    this.docMenuTab = tab;
    const host = this.docMenu.parentElement!.getBoundingClientRect();
    const box = tab.getBoundingClientRect();
    const x = at ? at.x : box.left;
    const y = at ? at.y : box.bottom;
    this.docMenu.style.left = `${Math.max(0, x - host.left)}px`;
    this.docMenu.style.top = `${Math.max(0, y - host.top)}px`;
    this.docMenu.hidden = false;
    remove.focus();
  }

  private closeDocMenu(restoreFocus: boolean): void {
    if (this.docMenu.hidden) return;
    this.docMenu.hidden = true;
    this.docMenu.replaceChildren();
    if (restoreFocus) this.docMenuTab?.focus();
    this.docMenuTab = null;
  }

  private deleteDoc(doc: ProposalDocRef, button: HTMLButtonElement): void {
    button.disabled = true;
    void this.removeDoc(doc).then(
      () => this.closeDocMenu(false),
      (e: unknown) => {
        button.disabled = false;
        const error = h("p", { class: "af-docs-menu-error" }, `Could not delete ${doc.file}: ${errorText(e)}`);
        this.docMenu.replaceChildren(button, error);
      },
    );
  }

  private async removeDoc(doc: ProposalDocRef): Promise<void> {
    const wasOpen = this.current?.name === doc.name;
    const suspended = this.suspendDoc(doc.name);
    try {
      await removeDoc(this.opts.sessionId, this.opts.slug, doc.name, this.opts.token);
    } catch (e) {
      this.finishDelete(doc.name, suspended, false);
      if (wasOpen && !this.disposed) void this.openDoc(doc, false, true);
      throw e;
    }
    this.finishDelete(doc.name, suspended, true);
    if (this.disposed) return;
    this.texts.delete(doc.name);
    if (wasOpen) {
      this.current = null;
      if (lastDoc.get(this.memoryKey()) === doc.name) lastDoc.delete(this.memoryKey());
    }
    this.ordered = this.ordered.filter((d) => d.name !== doc.name);
    this.renderStrip();
    this.renderQuestions();
    if (wasOpen) {
      const next = this.ordered[0];
      if (next) {
        void this.openDoc(next);
      } else {
        this.setStatus("saved");
        this.showNotice("This proposal has no docs yet. Add one with +.", null);
      }
    }
    this.scheduleListRefresh();
  }

  private onStripKey(e: KeyboardEvent, doc: ProposalDocRef): void {
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    e.preventDefault();
    const i = this.ordered.findIndex((d) => d.name === doc.name);
    const next = this.ordered[(i + (e.key === "ArrowRight" ? 1 : -1) + this.ordered.length) % this.ordered.length];
    if (next) {
      void this.openDoc(next).then(() => {
        (this.strip.querySelector('[aria-selected="true"]') as HTMLElement | null)?.focus();
      });
    }
  }

  private openMenu(): void {
    this.closeDocMenu(false);
    const items: HTMLElement[] = [];
    for (const name of this.info?.templates ?? []) {
      const item = h("button", { type: "button", class: "af-docs-menuitem", role: "menuitem" }, name);
      item.addEventListener("click", () => void this.createDoc(name));
      items.push(item);
    }
    const blank = h("button", { type: "button", class: "af-docs-menuitem", role: "menuitem" }, "Blank…");
    blank.addEventListener("click", () => this.showBlankForm());
    items.push(blank);
    this.menu.replaceChildren(...items);
    this.menu.hidden = false;
    this.addButton.setAttribute("aria-expanded", "true");
    items[0]?.focus();
  }

  private closeMenu(): void {
    this.menu.hidden = true;
    this.addButton.setAttribute("aria-expanded", "false");
  }

  private showBlankForm(): void {
    const input = h("input", { type: "text", class: "af-input af-docs-newname", placeholder: "doc-name" });
    input.setAttribute("aria-label", "New doc name");
    input.autocapitalize = "off";
    input.spellcheck = false;
    const error = h("p", { class: "af-docs-menu-error", hidden: true });
    const add = h("button", { type: "submit", class: "af-primary" }, "Add doc");
    const form = h("form", { class: "af-docs-newform" }, input, add, error);
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      const name = input.value.trim();
      if (!DOC_NAME_PATTERN.test(name)) {
        error.textContent = "Use lowercase letters, digits and dashes, starting with a letter or digit.";
        error.hidden = false;
        input.focus();
        return;
      }
      void this.createDoc(name, error);
    });
    this.menu.replaceChildren(form);
    input.focus();
  }

  private async createDoc(name: string, errorEl?: HTMLElement): Promise<void> {
    try {
      await addDoc(this.opts.sessionId, this.opts.slug, name, this.opts.token);
      const seq = this.pushSeq;
      const info = await getProposal(this.opts.sessionId, this.opts.slug, this.opts.token);
      if (this.disposed) return;
      this.closeMenu();
      this.applyInfo(info, seq);
      const doc = this.ordered.find((d) => d.name === name);
      if (doc) await this.openDoc(doc);
    } catch (e) {
      if (this.disposed) return;
      const message = `Could not add “${name}”: ${errorText(e)}`;
      if (errorEl) {
        errorEl.textContent = message;
        errorEl.hidden = false;
      } else {
        this.closeMenu();
        this.setStatus("error", message);
      }
    }
  }

  private ensureEditor(): DocsEditor {
    this.editor ??= createDocsEditor(this.editorHost, {
      text: "",
      label: "Doc text",
      onChange: (text) => {
        if (!this.current || this.freshBuffer) return;
        this.texts.set(this.current.name, text);
        this.renderQuestions();
        this.sync?.edit(text);
      },
    });
    return this.editor;
  }

  private async openDoc(doc: ProposalDocRef, toQuestion = false, force = false): Promise<void> {
    if (!force && this.current?.name === doc.name && this.sync) {
      if (toQuestion) this.editor?.goToFirstQuestion();
      return;
    }
    // Leaving a doc keeps its sync: unsaved edits are saved, and an unresolved
    // conflict draft stays until the doc is shown again.
    this.sync?.detach();
    this.sync = null;
    if (force) this.closeSync(doc.name, false);
    this.hideBanner();
    this.current = doc;
    lastDoc.set(this.memoryKey(), doc.name);
    this.renderStrip();
    const editor = this.ensureEditor();
    this.hideNotice();
    const kept = this.syncs.get(doc.name);
    if (kept?.loaded) {
      this.sync = kept;
      this.freshBuffer = false;
      const shown = kept.attach();
      editor.setText(shown.text);
      editor.setEditable(true);
      this.texts.set(doc.name, shown.text);
      this.renderQuestions();
      if (toQuestion) editor.goToFirstQuestion();
      return;
    }
    this.closeSync(doc.name, false);
    this.setStatus("loading");
    // Nothing can be typed until this doc's text has arrived: the editor shows
    // nothing of the previous doc and takes no keystrokes.
    editor.setText("");
    editor.setEditable(false);
    this.freshBuffer = true;
    const key = draftKey(this.opts.sessionId, this.opts.slug, doc.name);
    const sync = new DocSync({
      load: async () => {
        // Wait for every tab that closed on this doc with a save still in flight.
        const priors = [...(closing.get(key) ?? [])].filter((p) => p !== sync);
        await Promise.all(priors.map((p) => p.whenQuiet()));
        return getDoc(this.opts.sessionId, this.opts.slug, doc.name, this.opts.token);
      },
      save: (text, base, final) => putDoc(this.opts.sessionId, this.opts.slug, doc.name, text, base,
        this.opts.token, final === true),
      setBuffer: (text) => {
        if (this.sync !== sync || !this.editor) return;
        if (this.freshBuffer) {
          this.freshBuffer = false;
          this.editor.setText(text);
          this.editor.setEditable(true);
        } else {
          this.editor.applyRemote(diffText(this.editor.getText(), text));
        }
        this.texts.set(doc.name, text);
        this.renderQuestions();
      },
      onStatus: (status, detail) => {
        if (this.sync === sync) this.setStatus(status, detail);
      },
      draft: draftStore(key),
      setTimer: (fn, ms) => window.setTimeout(fn, ms),
      clearTimer: (handle) => window.clearTimeout(handle as number),
    });
    this.syncs.set(doc.name, sync);
    this.sync = sync;
    try {
      await sync.start();
    } catch (e) {
      if (this.sync !== sync) return;
      this.showNotice(`Could not load ${doc.file}: ${errorText(e)}`, () => void this.openDoc(doc, toQuestion, true));
      return;
    }
    if (toQuestion && this.sync === sync) editor.goToFirstQuestion();
  }

  private async refreshText(doc: ProposalDocRef): Promise<void> {
    try {
      const d = await getDoc(this.opts.sessionId, this.opts.slug, doc.name, this.opts.token);
      if (this.disposed || this.current?.name === doc.name) return;
      this.texts.set(doc.name, d.text);
      this.renderQuestions();
    } catch {
      // The count falls back to the daemon's total until a later event succeeds.
    }
  }

  private handleChange(change: ProposalChange): void {
    if (this.disposed || change.slug !== this.opts.slug) return;
    // Any change can add or delete a doc, or move the open-question total.
    this.scheduleListRefresh();
    switch (changeAction(change, this.opts.slug, this.current, this.ordered)) {
      case "open-doc":
        this.sync?.remoteChanged(change.commit);
        break;
      case "other-doc": {
        const doc = this.ordered.find((d) => changeNamesDoc(change, d));
        if (doc) {
          // A doc that is not shown keeps its sync, so it still learns of the change.
          this.syncs.get(doc.name)?.remoteChanged(change.commit);
          void this.refreshText(doc);
        }
        break;
      }
      case "doc-list":
      case "ignore":
        break;
    }
  }

  private scheduleListRefresh(): void {
    if (this.refreshTimer !== null) window.clearTimeout(this.refreshTimer);
    this.refreshTimer = window.setTimeout(() => {
      this.refreshTimer = null;
      const seq = this.pushSeq;
      void getProposal(this.opts.sessionId, this.opts.slug, this.opts.token).then((info) => {
        if (this.disposed) return;
        const known = new Set(this.ordered.map((d) => d.name));
        this.applyInfo(info, seq);
        for (const doc of this.ordered) {
          if (!known.has(doc.name)) void this.refreshText(doc);
        }
        const current = this.current;
        for (const name of [...this.syncs.keys()]) {
          // The agent or another window deleted the doc: its syncs end for good.
          if (!this.ordered.some((d) => d.name === name)) this.finishDelete(name, this.suspendDoc(name), true);
        }
        if (current && !this.ordered.some((d) => d.name === current.name)) {
          this.current = null;
        }
        if (!this.current && this.ordered[0]) {
          void this.openDoc(this.ordered[0]);
        } else if (!this.current) {
          this.showNotice("This proposal has no docs yet. Add one with +.", null);
        }
      }, () => undefined);
    }, REFRESH_DEBOUNCE_MS);
  }

  private applyPush(status: PushStatus | null): void {
    this.pushSeq++;
    this.chip.apply(status);
  }

  private renderPush(view: PushView | null): void {
    this.push.hidden = view === null;
    if (!view) return;
    this.push.dataset.state = view.state;
    this.chipText.textContent = view.text;
    this.chipText.title = view.text;
    this.chipText.hidden = view.text === "";
    this.chipAction.hidden = view.action === null;
    this.chipAction.disabled = !view.actionEnabled;
    this.chipSep.hidden = view.text === "" || view.action === null;
    if (view.action === "hold") {
      this.chipAction.textContent = "Hold";
      this.chipAction.title = "Hold the send until your next edit";
    } else if (view.action === "send") {
      this.chipAction.textContent = "Send now";
      this.chipAction.title = view.unsent > 0
        ? `Send ${view.unsent === 1 ? "1 edit" : `${view.unsent} edits`} to the agent`
        : "No edits to send";
    }
  }

  private runChipAction(): void {
    const status = this.chip.current;
    if (!status) return;
    if (status.state === "countdown" && status.mode === "auto") {
      this.runPush("hold", () => holdPush(this.opts.sessionId, this.opts.token));
    } else {
      this.runPush("send", async () => {
        // The daemon builds the prompt from committed edits, so every open doc is
        // flushed and its save awaited first: Send now never goes out ahead of a
        // newer edit.
        const verdicts = await Promise.all([...this.syncs.values()].map((s) => s.whenSaved()));
        if (verdicts.includes("blocked")) {
          throw new Error("some edits are not saved yet; resolve them first");
        }
        return sendChanges(this.opts.sessionId, this.opts.token);
      });
    }
  }

  private runPush(what: string, call: () => Promise<PushStatus>): void {
    this.chip.setBusy(true);
    call().then(
      (status) => {
        if (this.disposed) return;
        this.chip.setBusy(false);
        this.applyPush(status);
      },
      (e: unknown) => {
        if (this.disposed) return;
        this.chip.setBusy(false);
        this.chip.setError(`Could not ${what} · ${errorText(e)}`);
      },
    );
  }

  private questionCounts(): Map<string, number> {
    const counts = new Map<string, number>();
    for (const doc of this.ordered) {
      const text = this.texts.get(doc.name);
      if (text !== undefined) counts.set(doc.name, countOpenQuestions(text));
    }
    return counts;
  }

  private renderQuestions(): void {
    const counts = this.questionCounts();
    const allKnown = this.ordered.every((d) => counts.has(d.name));
    let total = 0;
    for (const n of counts.values()) total += n;
    if (!allKnown) total = Math.max(total, this.info?.open_questions ?? 0);
    this.questions.hidden = total === 0;
    const label = questionCountLabel(total);
    this.questions.replaceChildren(icon("circle-question"), h("span", {}, String(total)));
    this.questions.title = `${label} · go to the next one`;
    this.questions.setAttribute("aria-label", `${label}. Go to the next one`);
  }

  private async goToNextQuestion(): Promise<void> {
    if (this.editor && this.current && this.editor.goToNextQuestion(false)) return;
    const target = nextQuestionDoc(this.ordered, this.current, this.questionCounts(), false);
    if (!target) return;
    if (target.here) {
      this.editor?.goToNextQuestion(true);
    } else {
      await this.openDoc(target.doc, true);
    }
  }

  private setStatus(status: SyncStatus, detail = ""): void {
    const text = statusText(status, detail);
    this.statusText.textContent = text;
    this.status.title = text;
    this.status.hidden = text === "";
    this.status.dataset.state = status;
    if (status === "conflict") {
      this.showBanner();
    } else if (!this.banner.hidden) {
      this.hideBanner();
    }
  }

  private showBanner(): void {
    const sync = this.sync;
    if (!sync) return;
    const action = (label: string, run: () => Promise<void>): HTMLButtonElement => {
      const btn = h("button", { type: "button", class: "af-ghost af-docs-banner-action" }, label);
      btn.addEventListener("click", () => {
        for (const b of this.banner.querySelectorAll("button")) b.disabled = true;
        run().catch((e: unknown) => {
          for (const b of this.banner.querySelectorAll("button")) b.disabled = false;
          this.statusText.textContent = `Not saved · ${errorText(e)}`;
          this.status.title = this.statusText.textContent;
          this.status.dataset.state = "error";
        });
      });
      return btn;
    };
    const sep = (): HTMLElement => h("span", { class: "af-docs-sep", ariaHidden: "true" }, " · ");
    this.banner.replaceChildren(
      h("span", { class: "af-docs-banner-text" }, "The agent changed this section too"),
      sep(),
      action("Keep mine", () => sync.keepMine()),
      sep(),
      action("Take theirs", () => sync.takeTheirs()),
      sep(),
      action("Show both", () => sync.showBoth()),
    );
    this.banner.hidden = false;
  }

  private hideBanner(): void {
    this.banner.hidden = true;
    this.banner.replaceChildren();
  }
}
