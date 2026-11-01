// Session previews for the Backlog view (backlog v2 proposal, technical.md
// "Session preview"): /v1/PreviewSession text, cached for 5 s, one request at a
// time, and a hover popover on the state chip that waits 300 ms before it asks.

import { af, errorText } from "./api.js";
import { h } from "./dom.js";

export const PREVIEW_DELAY_MS = 300;
export const PREVIEW_TTL_MS = 5000;
export const PREVIEW_LINES = 12;

export interface Preview {
  text: string;
  error?: string;
}

export async function previewSession(id: string, lines: number, token: string): Promise<string> {
  const resp = await af<{ text?: string }>("PreviewSession", { id, lines }, token);
  return resp?.text ?? "";
}

interface Waiting {
  id: string;
  keep: boolean;
  resolve: (p: Preview | null) => void;
}

/** Previews by session id, one request at a time; the rest wait in order. A
 *  hover ask replaces a waiting hover ask, which then resolves null. A kept ask
 *  (the panel's) is never replaced, and does not replace a hover ask either. */
export class PreviewCache {
  private readonly entries = new Map<string, { preview: Preview; at: number }>();
  private busy = false;
  private waiting: Waiting[] = [];

  constructor(private readonly load: (id: string) => Promise<string>, private readonly now: () => number = Date.now) {}

  cached(id: string): Preview | undefined {
    const e = this.entries.get(id);
    return e && this.now() - e.at < PREVIEW_TTL_MS ? e.preview : undefined;
  }

  get(id: string, keep = false): Promise<Preview | null> {
    const hit = this.cached(id);
    if (hit) return Promise.resolve(hit);
    if (!this.busy) return this.run(id);
    return new Promise((resolve) => {
      const same = this.waiting.find((w) => w.id === id);
      if (same) {
        const prev = same.resolve;
        same.resolve = (p) => { prev(p); resolve(p); };
        same.keep ||= keep;
        return;
      }
      if (!keep) {
        for (const w of this.waiting.filter((x) => !x.keep)) w.resolve(null);
        this.waiting = this.waiting.filter((x) => x.keep);
      }
      this.waiting.push({ id, keep, resolve });
    });
  }

  private async run(id: string): Promise<Preview> {
    this.busy = true;
    let preview: Preview;
    try {
      preview = { text: await this.load(id) };
    } catch (e) {
      preview = { text: "", error: errorText(e) };
    }
    this.entries.set(id, { preview, at: this.now() });
    this.busy = false;
    this.drain();
    return preview;
  }

  private drain(): void {
    while (!this.busy && this.waiting.length > 0) {
      const w = this.waiting.shift()!;
      const hit = this.cached(w.id);
      if (hit) w.resolve(hit);
      else void this.run(w.id).then(w.resolve);
    }
  }
}

/** One floating preview for the list's state chips. */
export class PreviewPopover {
  readonly el: HTMLElement;
  private readonly heading = h("div", { class: "af-backlog-preview-head" });
  private readonly body = h("pre", { class: "af-backlog-preview-text" });
  private timer: ReturnType<typeof setTimeout> | null = null;
  private anchor: HTMLElement | null = null;
  private sessionId = "";

  constructor(private readonly cache: PreviewCache) {
    this.el = h("div", { class: "af-backlog-preview", role: "tooltip", hidden: true }, this.heading, this.body);
    this.el.id = "af-backlog-preview";
  }

  /** Wires an anchor to show the session's preview on hover, and on focus of
   *  focusEl (its row, for keyboard users). */
  attach(anchor: HTMLElement, sessionId: string, heading: string, focusEl: HTMLElement = anchor): void {
    anchor.dataset.previewSession = sessionId;
    const show = (): void => this.schedule(anchor, sessionId, heading);
    const hide = (): void => {
      if (this.anchor === anchor) this.hide();
    };
    anchor.addEventListener("mouseenter", show);
    focusEl.addEventListener("focus", show);
    anchor.addEventListener("mouseleave", hide);
    // A row removed by a rebuild may blur; rebind() decides for that one.
    focusEl.addEventListener("blur", () => {
      if (anchor.isConnected) hide();
    });
  }

  /** After a list rebuild: follows the open preview to its new chip, or hides it. */
  rebind(root: HTMLElement): void {
    if (!this.anchor) return;
    const next = [...root.querySelectorAll<HTMLElement>("[data-preview-session]")]
      .find((el) => el.dataset.previewSession === this.sessionId);
    if (!next) {
      this.hide();
      return;
    }
    this.anchor = next;
    if (!this.el.hidden) next.setAttribute("aria-describedby", this.el.id);
  }

  hide(): void {
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
    this.anchor?.removeAttribute("aria-describedby");
    this.anchor = null;
    this.sessionId = "";
    this.el.hidden = true;
  }

  private schedule(anchor: HTMLElement, sessionId: string, heading: string): void {
    if (this.anchor === anchor) return;
    this.hide();
    this.anchor = anchor;
    this.sessionId = sessionId;
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.cache.get(sessionId).then((p) => {
        if (!p || this.sessionId !== sessionId || !this.anchor?.isConnected) return;
        this.show(this.anchor, heading, p);
      });
    }, PREVIEW_DELAY_MS);
  }

  private show(anchor: HTMLElement, heading: string, p: Preview): void {
    this.heading.textContent = heading;
    this.body.textContent = p.error ? `Preview unavailable · ${p.error}` : p.text || "No output yet";
    this.body.classList.toggle("af-backlog-preview-error", Boolean(p.error));
    const r = anchor.getBoundingClientRect();
    const vw = typeof window === "undefined" ? 0 : window.innerWidth;
    const vh = typeof window === "undefined" ? 0 : window.innerHeight;
    const below = vh === 0 || r.top + r.height < vh * 0.6;
    this.el.style.top = below ? `${r.top + r.height}px` : "auto";
    this.el.style.bottom = below ? "auto" : `${vh - r.top}px`;
    const left = Math.max(0, r.left);
    const end = Math.min(vw || Infinity, r.left + r.width);
    const fromRight = vw > 0 && end > vw / 2;
    this.el.style.left = fromRight ? "auto" : `${left}px`;
    this.el.style.right = fromRight ? `${Math.max(0, vw - end)}px` : "auto";
    const room = fromRight ? Math.max(0, end) : vw > 0 ? vw - left : 0;
    this.el.style.maxWidth = room > 0 ? `min(calc(var(--af-space-4) * 36), ${room}px)` : "";
    this.el.hidden = false;
    anchor.setAttribute("aria-describedby", this.el.id);
  }
}
