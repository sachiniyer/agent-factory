// A small DOM for the Backlog view's unit tests; the web tests run in node with
// no DOM library. It models what the view's focus handling depends on: removing
// a node that holds focus blurs it, as a browser does.

type Listener = (e: FakeEvent) => void;

export class FakeEvent {
  target: FakeElement | null = null;
  currentTarget: FakeElement | null = null;
  defaultPrevented = false;
  stopped = false;
  key = "";
  altKey = false;
  ctrlKey = false;
  metaKey = false;
  shiftKey = false;
  isComposing = false;
  clientY = 0;
  relatedTarget: FakeNode | null = null;
  dataTransfer: FakeDataTransfer | null = null;
  constructor(readonly type: string, init: Partial<FakeEvent> = {}, readonly bubbles = true) {
    Object.assign(this, init);
  }
  preventDefault(): void {
    this.defaultPrevented = true;
  }
  stopPropagation(): void {
    this.stopped = true;
  }
}

export class FakeDataTransfer {
  readonly data = new Map<string, string>();
  effectAllowed = "";
  get types(): string[] {
    return [...this.data.keys()];
  }
  setData(type: string, value: string): void {
    this.data.set(type, value);
  }
}

class FakeNode {
  parentNode: FakeElement | null = null;
  constructor(readonly doc: FakeDocument) {}
  get isConnected(): boolean {
    let n: FakeNode | null = this;
    while (n?.parentNode) n = n.parentNode;
    return n === this.doc.body;
  }
  get textContent(): string {
    return "";
  }
  remove(): void {
    this.parentNode?.removeChild(this);
  }
}

class FakeText extends FakeNode {
  constructor(doc: FakeDocument, public data: string) {
    super(doc);
  }
  override get textContent(): string {
    return this.data;
  }
}

interface Compound {
  tag: string | null;
  classes: string[];
  attrs: [string, string | null][];
}

function splitTop(selector: string): string[] {
  const parts: string[] = [];
  let cur = "";
  let quote = "";
  let depth = 0;
  for (const ch of selector) {
    if (quote) {
      cur += ch;
      if (ch === quote) quote = "";
    } else if (ch === "\"" || ch === "'") {
      quote = ch;
      cur += ch;
    } else if (ch === "[") {
      depth++;
      cur += ch;
    } else if (ch === "]") {
      depth--;
      cur += ch;
    } else if (/\s/.test(ch) && depth === 0) {
      if (cur) parts.push(cur);
      cur = "";
    } else {
      cur += ch;
    }
  }
  if (cur) parts.push(cur);
  return parts;
}

function parseCompound(s: string): Compound {
  const out: Compound = { tag: null, classes: [], attrs: [] };
  const tag = /^[a-zA-Z][\w-]*/.exec(s);
  let rest = s;
  if (tag) {
    out.tag = tag[0].toUpperCase();
    rest = s.slice(tag[0].length);
  }
  const re = /\.([\w-]+)|\[([\w-]+)(?:="((?:[^"\\]|\\.)*)")?\]/gy;
  let m: RegExpExecArray | null;
  while (rest && (m = re.exec(rest))) {
    if (m[1]) out.classes.push(m[1]);
    else out.attrs.push([m[2], m[3] === undefined ? null : m[3].replace(/\\(.)/g, "$1")]);
    if (re.lastIndex === rest.length) break;
  }
  if (rest && re.lastIndex !== rest.length) throw new Error(`fake DOM cannot parse selector ${s}`);
  return out;
}

const kebab = (k: string): string => k.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`);

export class FakeElement extends FakeNode {
  readonly tagName: string;
  childNodes: FakeNode[] = [];
  readonly attrs = new Map<string, string>();
  readonly listeners = new Map<string, Listener[]>();
  readonly style: Record<string, string> = {};
  value = "";
  checked = false;
  disabled = false;
  hidden = false;
  type = "";
  title = "";
  placeholder = "";
  href = "";
  target = "";
  rel = "";
  draggable = false;
  open = false;
  rows = 0;
  readonly dataset: Record<string, string>;
  readonly classList = {
    add: (...c: string[]) => this.setClasses([...new Set([...this.classes(), ...c])]),
    remove: (...c: string[]) => this.setClasses(this.classes().filter((x) => !c.includes(x))),
    contains: (c: string) => this.classes().includes(c),
    toggle: (c: string, on = !this.classes().includes(c)) => {
      if (on) this.classList.add(c);
      else this.classList.remove(c);
      return on;
    },
  };

  constructor(doc: FakeDocument, tag: string) {
    super(doc);
    this.tagName = tag.toUpperCase();
    this.dataset = new Proxy({} as Record<string, string>, {
      get: (_, k) => this.getAttribute(`data-${kebab(String(k))}`) ?? undefined,
      set: (_, k, v) => {
        this.setAttribute(`data-${kebab(String(k))}`, String(v));
        return true;
      },
    });
  }

  private classes(): string[] {
    return (this.attrs.get("class") ?? "").split(/\s+/).filter(Boolean);
  }
  private setClasses(c: string[]): void {
    this.attrs.set("class", c.join(" "));
  }
  get className(): string {
    return this.attrs.get("class") ?? "";
  }
  set className(v: string) {
    this.attrs.set("class", v);
  }
  get tabIndex(): number {
    const v = this.attrs.get("tabindex");
    return v === undefined ? -1 : Number(v);
  }
  set tabIndex(v: number) {
    this.attrs.set("tabindex", String(v));
  }
  set ariaHidden(v: string) {
    this.attrs.set("aria-hidden", v);
  }
  setAttribute(k: string, v: string): void {
    this.attrs.set(k, String(v));
  }
  getAttribute(k: string): string | null {
    return this.attrs.get(k) ?? null;
  }
  removeAttribute(k: string): void {
    this.attrs.delete(k);
  }
  hasAttribute(k: string): boolean {
    return this.attrs.has(k);
  }

  get children(): FakeElement[] {
    return this.childNodes.filter((n): n is FakeElement => n instanceof FakeElement);
  }
  get firstChild(): FakeNode | null {
    return this.childNodes[0] ?? null;
  }
  private sibling(step: number): FakeElement | null {
    const sibs = this.parentNode?.children ?? [];
    return sibs[sibs.indexOf(this) + step] ?? null;
  }
  get nextElementSibling(): FakeElement | null {
    return this.sibling(1);
  }
  get previousElementSibling(): FakeElement | null {
    return this.sibling(-1);
  }
  override get textContent(): string {
    return this.childNodes.map((n) => n.textContent).join("");
  }
  override set textContent(v: string) {
    this.replaceChildren(v);
  }

  append(...nodes: (FakeNode | string)[]): void {
    for (const n of nodes) {
      const node = typeof n === "string" ? new FakeText(this.doc, n) : n;
      node.remove();
      node.parentNode = this;
      this.childNodes.push(node);
    }
  }
  replaceChildren(...nodes: (FakeNode | string)[]): void {
    for (const c of [...this.childNodes]) this.removeChild(c);
    this.append(...nodes);
  }
  removeChild(n: FakeNode): void {
    const i = this.childNodes.indexOf(n);
    if (i < 0) return;
    this.childNodes.splice(i, 1);
    n.parentNode = null;
    const active = this.doc.activeElement;
    if (n instanceof FakeElement && active !== this.doc.body && n.contains(active)) this.doc.activeElement = this.doc.body;
  }
  contains(n: FakeNode | null): boolean {
    for (let c: FakeNode | null = n; c; c = c.parentNode) if (c === this) return true;
    return false;
  }

  addEventListener(type: string, fn: Listener): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn]);
  }
  removeEventListener(type: string, fn: Listener): void {
    this.listeners.set(type, (this.listeners.get(type) ?? []).filter((f) => f !== fn));
  }
  dispatchEvent(e: FakeEvent): boolean {
    e.target = this;
    for (let el: FakeElement | null = this; el && !e.stopped; el = e.bubbles ? el.parentNode : null) {
      e.currentTarget = el;
      for (const fn of el.listeners.get(e.type) ?? []) fn(e);
    }
    return !e.defaultPrevented;
  }
  click(): void {
    if (!this.disabled) this.dispatchEvent(new FakeEvent("click"));
  }
  focus(): void {
    if (this.doc.activeElement === this) return;
    this.doc.activeElement = this;
    this.dispatchEvent(new FakeEvent("focus", {}, false));
  }
  blur(): void {
    if (this.doc.activeElement === this) this.doc.activeElement = this.doc.body;
  }
  getBoundingClientRect(): { top: number; height: number; left: number; width: number } {
    return { top: 0, height: 20, left: 0, width: 200 };
  }

  matches(selector: string): boolean {
    const chain = splitTop(selector).map(parseCompound);
    const one = (el: FakeElement, c: Compound): boolean =>
      (c.tag === null || el.tagName === c.tag) &&
      c.classes.every((k) => el.classList.contains(k)) &&
      c.attrs.every(([k, v]) => (v === null ? el.hasAttribute(k) : el.getAttribute(k) === v));
    if (!one(this, chain[chain.length - 1])) return false;
    let el: FakeElement | null = this.parentNode;
    for (let i = chain.length - 2; i >= 0; i--) {
      while (el && !one(el, chain[i])) el = el.parentNode;
      if (!el) return false;
      el = el.parentNode;
    }
    return true;
  }
  closest(selector: string): FakeElement | null {
    for (let el: FakeElement | null = this; el; el = el.parentNode) if (el.matches(selector)) return el;
    return null;
  }
  querySelectorAll(selector: string): FakeElement[] {
    const out: FakeElement[] = [];
    const walk = (el: FakeElement): void => {
      for (const c of el.children) {
        if (c.matches(selector)) out.push(c);
        walk(c);
      }
    };
    walk(this);
    return out;
  }
  querySelector(selector: string): FakeElement | null {
    return this.querySelectorAll(selector)[0] ?? null;
  }
}

export class FakeDocument {
  readonly body: FakeElement;
  readonly documentElement: FakeElement;
  activeElement: FakeElement;
  constructor() {
    this.documentElement = new FakeElement(this, "html");
    this.body = new FakeElement(this, "body");
    this.activeElement = this.body;
  }
  createElement(tag: string): FakeElement {
    return new FakeElement(this, tag);
  }
  createElementNS(_ns: string, tag: string): FakeElement {
    return new FakeElement(this, tag);
  }
  createTextNode(text: string): FakeText {
    return new FakeText(this, text);
  }
}

/** Installs a fresh document and CSS.escape; the returned function restores the old globals. */
export function installFakeDom(): { doc: FakeDocument; restore(): void } {
  const g = globalThis as Record<string, unknown>;
  const prev = { document: g.document, CSS: g.CSS };
  const doc = new FakeDocument();
  g.document = doc;
  g.CSS = { escape: (s: string) => s.replace(/["\\]/g, "\\$&") };
  return {
    doc,
    restore: () => {
      g.document = prev.document;
      g.CSS = prev.CSS;
    },
  };
}
