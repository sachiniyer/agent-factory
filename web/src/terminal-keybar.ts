import { TerminalSoftInput } from "./terminal-soft-input.js";

/** Phone terminal controls and xterm-compatible key encodings. */
export function keyBytes(key: string, ctrl = false, alt = false, applicationCursor = false): string {
  const arrows: Record<string, string> = { "←": "D", "↑": "A", "↓": "B", "→": "C" };
  const special: Record<string, string> = { Esc: "\x1b", Tab: "\t", "^C": "\x03" };
  if (arrows[key]) {
    const modifier = 1 + (alt ? 2 : 0) + (ctrl ? 4 : 0);
    // Modified arrows always use CSI, including in application-cursor mode.
    return modifier > 1 ? `\x1b[1;${modifier}${arrows[key]}`
      : `\x1b${applicationCursor ? "O" : "["}${arrows[key]}`;
  }
  // Ctrl+Tab has no legacy byte form: send Tab and consume the one-shot.
  // Esc and ^C already encode control bytes; Alt prefixes all three with ESC.
  if (special[key]) return (alt ? "\x1b" : "") + special[key];
  const code = key.toUpperCase().charCodeAt(0);
  const text = ctrl && /^[\x40-\x7f]$/.test(key) && code >= 64 && code <= 95 ? String.fromCharCode(code & 31) : key;
  return (alt ? "\x1b" : "") + text;
}

export const KEYBAR_ROWS = [["Ctrl", "Alt", "Esc", "Tab", "^C", "Arrows"], ["More keys", "←", "↑", "↓", "→"]] as const;

interface DecodedKeybarInput {
  key: string;
  modifierBits: number;
  applicationCursor: boolean;
  arrowSuffix?: string;
}

// This is the inverse of keyBytes for every actionable key in KEYBAR_ROWS.
// State and navigation buttons are the only entries that do not emit input.
const decodedKeybarInput = new Map<string, DecodedKeybarInput>();
const nonInputKeys = new Set(["Ctrl", "Alt", "Arrows", "More keys"]);
const arrowSuffixes: Record<string, string> = { "←": "D", "↑": "A", "↓": "B", "→": "C" };
for (const key of KEYBAR_ROWS.flat().filter(value => !nonInputKeys.has(value))) {
  const arrowSuffix = arrowSuffixes[key];
  if (arrowSuffix) {
    decodedKeybarInput.set(keyBytes(key), { key, modifierBits: 0, applicationCursor: false, arrowSuffix });
    decodedKeybarInput.set(keyBytes(key, false, false, true), { key, modifierBits: 0, applicationCursor: true, arrowSuffix });
    for (let modifierBits = 1; modifierBits <= 15; modifierBits++) {
      decodedKeybarInput.set(`\x1b[1;${modifierBits + 1}${arrowSuffix}`,
        { key, modifierBits, applicationCursor: false, arrowSuffix });
    }
    continue;
  }
  for (const ctrl of [false, true]) {
    for (const alt of [false, true]) {
      const bytes = keyBytes(key, ctrl, alt);
      if (!decodedKeybarInput.has(bytes))
        decodedKeybarInput.set(bytes, { key, modifierBits: (ctrl ? 4 : 0) | (alt ? 2 : 0), applicationCursor: false });
    }
  }
}

type Modifier = "Ctrl" | "Alt";
type State = "off" | "once" | "locked";
export class StickyModifiers {
  private values: Record<Modifier, State> = { Ctrl: "off", Alt: "off" };
  private tapped: Record<Modifier, number> = { Ctrl: -Infinity, Alt: -Infinity };
  state(key: Modifier): State { return this.values[key]; }
  tap(key: Modifier, now: number): void {
    this.values[key] = this.values[key] === "off" ? "once"
      : this.values[key] === "once" && now - this.tapped[key] <= 350 ? "locked" : "off";
    this.tapped[key] = now;
  }
  reset(): void {
    this.values = { Ctrl: "off", Alt: "off" };
    this.tapped = { Ctrl: -Infinity, Alt: -Infinity };
  }
  key(key: string, applicationCursor = false): string {
    const result = keyBytes(key, this.values.Ctrl !== "off", this.values.Alt !== "off", applicationCursor);
    this.consumeOnce();
    return result;
  }
  input(text: string, source: "terminal" | "user" = "terminal"): string {
    // Decode only genuine user emissions from keybar's complete input vocabulary.
    // Parser replies remain byte-for-byte, while bar and hardware origins converge.
    const decoded = source === "user" ? decodedKeybarInput.get(text) : undefined;
    if (decoded) {
      const combined = decoded.modifierBits |
        (this.values.Alt !== "off" ? 2 : 0) | (this.values.Ctrl !== "off" ? 4 : 0);
      const result = decoded.arrowSuffix
        ? combined ? `\x1b[1;${combined + 1}${decoded.arrowSuffix}`
          : keyBytes(decoded.key, false, false, decoded.applicationCursor)
        : keyBytes(decoded.key, (combined & 4) !== 0, (combined & 2) !== 0);
      this.consumeOnce();
      return result;
    }
    // Unrecognized user controls retain the consume-and-pass-through fallback.
    if (!text || text.charCodeAt(0) < 32 || text.charCodeAt(0) === 127) {
      if (text && source === "user") this.consumeOnce();
      return text;
    }
    return Array.from(text, char => this.key(char)).join("");
  }
  private consumeOnce(): void {
    for (const modifier of ["Ctrl", "Alt"] as const) if (this.values[modifier] === "once") this.values[modifier] = "off";
  }
}

export function keybarPointerDown(event: Pick<Event, "preventDefault">, act: () => void): void {
  event.preventDefault(); // Cancel the browser's button-focus default before acting.
  act();
}

export class TerminalKeybar {
  private arrows = false;
  private readonly rows: HTMLElement[] = [];

  private readonly modifiers = new StickyModifiers();
  private readonly bar = document.createElement("div");
  private readonly phone = window.matchMedia("(max-width: 768px)");
  private readonly viewport = window.visualViewport;
  private readonly observer: ResizeObserver;
  private readonly textarea: (EventTarget & { value?: string }) | null;
  private focused = false;
  private physicalInput = false;
  private readonly onKeyDown = (event: KeyboardEvent): void => {
    this.physicalInput = event.key.length === 1 && !event.isComposing && event.keyCode !== 229;
    // CompositionHelper emits textarea-diff input from a zero-delay callback,
    // after onKey's synchronous user-input marker would normally be available.
    if (event.keyCode === 229) this.markUserInput(true);
  };
  private readonly onKeyUp = (): void => { this.physicalInput = false; };
  private readonly softInput: TerminalSoftInput;
  private readonly buttons = new Map<Modifier, HTMLButtonElement>();
  private readonly originalMaxHeight: string;
  private userInput = false;
  private userInputGeneration = 0;
  private deferred229: { before: string; generation: number } | undefined;
  private deferred229Generation = 0;

  constructor(private readonly host: HTMLElement, private readonly input: (data: string) => void,
    private readonly refit: () => void, private readonly applicationCursor: () => boolean) {
    this.originalMaxHeight = host.style.maxHeight;
    this.textarea = host.querySelector<HTMLTextAreaElement>(".xterm-helper-textarea");
    this.bar.className = "af-terminal-keybar";
    this.bar.setAttribute("role", "group");
    this.bar.setAttribute("aria-label", "Terminal keys");
    for (const keys of KEYBAR_ROWS) {
      const row = document.createElement("div");
      row.className = "af-keybar-row";
      this.rows.push(row);
      for (const key of keys) {
        const button = document.createElement("button");
        button.type = "button";
        button.textContent = key;
        button.setAttribute("aria-label", key === "^C" ? "Interrupt (^C)" : key);
        const act = () => {
          if (!this.focused || !this.phone.matches) return;
          if (key === "Arrows" || key === "More keys") this.arrows = key === "Arrows";
          else if (key === "Ctrl" || key === "Alt") this.modifiers.tap(key, performance.now());
          // Resolve and consume at source, then enter xterm's user-input path.
          // transform() preserves these control/escape bytes without reapplying.
          else this.sendUserInput(this.modifiers.key(key, this.applicationCursor()));
          this.paint();
        };
        button.addEventListener("pointerdown", event => keybarPointerDown(event, act));
        // Assistive activation has no pointerdown. Compatibility clicks must not
        // send twice (or turn a one-shot tap into a locked modifier).
        button.addEventListener("click", event => { if (event.detail === 0) act(); });
        if (key === "Ctrl" || key === "Alt") this.buttons.set(key, button);
        row.append(button);
      }
      this.bar.append(row);
    }
    this.observer = new ResizeObserver(this.layout);
    this.observer.observe(this.bar);
    this.observer.observe(host);
    this.phone.addEventListener("change", this.layout);
    this.viewport?.addEventListener("resize", this.layout);
    this.viewport?.addEventListener("scroll", this.layout);
    window.addEventListener("resize", this.layout);
    host.addEventListener("keydown", this.onKeyDown, true);
    host.addEventListener("keyup", this.onKeyUp, true);
    this.softInput = new TerminalSoftInput(host, this.textarea,
      () => this.focused && this.phone.matches, () => this.physicalInput, data => this.sendUserInput(data),
      () => this.modifiers.state("Ctrl") !== "off" || this.modifiers.state("Alt") !== "off");
    this.paint();
  }

  setFocused(focused: boolean): void {
    this.focused = focused;
    if (!focused) {
      this.modifiers.reset(); this.softInput.reset(); this.physicalInput = false; this.arrows = false;
      this.deferred229 = undefined; this.deferred229Generation += 1;
    }
    this.paint();
    this.layout();
  }
  transform(text: string): string {
    const source = this.userInput || this.takeDeferred229(text) ? "user" : "terminal";
    this.userInput = false;
    this.userInputGeneration += 1;
    const output = this.softInput.transform(text, value =>
      this.focused && this.phone.matches ? this.modifiers.input(value, source) : value);
    this.paint();
    return output;
  }
  /** Mark xterm's synchronous emission, or its matching 229 textarea diff, as user input. */
  markUserInput(deferred = false): void {
    if (deferred) {
      const before = this.textarea?.value;
      if (before === undefined) return;
      const generation = ++this.deferred229Generation;
      this.deferred229 = { before, generation };
      // Queue expiry behind CompositionHelper's setTimeout(0), but do not mark
      // an unrelated parser reply as user input while that callback is pending.
      queueMicrotask(() => setTimeout(() => {
        if (this.deferred229?.generation === generation) this.deferred229 = undefined;
      }, 0));
      return;
    }
    const generation = ++this.userInputGeneration;
    this.userInput = true;
    const clear = () => {
      if (this.userInputGeneration === generation) this.userInput = false;
    };
    // Synchronous onKey and term.input paths own only the current event turn.
    queueMicrotask(clear);
  }
  private takeDeferred229(text: string): boolean {
    const pending = this.deferred229;
    const value = this.textarea?.value;
    if (!pending || value === undefined) return false;
    const diff = value.replace(pending.before, "");
    const expected = value.length > pending.before.length ? diff
      : value.length < pending.before.length ? "\x7f"
        : value !== pending.before ? value : undefined;
    if (text !== expected) return false;
    this.deferred229 = undefined;
    this.deferred229Generation += 1;
    return true;
  }
  private sendUserInput(data: string): void {
    this.markUserInput();
    this.input(data);
  }
  private paint(): void {
    this.rows.forEach((row, index) => { row.hidden = index !== (this.arrows ? 1 : 0); });
    for (const [key, button] of this.buttons) {
      const state = this.modifiers.state(key);
      button.dataset.state = state;
      button.setAttribute("aria-pressed", String(state !== "off"));
      button.setAttribute("aria-description", state === "locked" ? "Locked; tap to release" : state === "once" ? "Next key" : "Double tap to lock");
      button.title = state === "locked" ? `${key} locked · Tap to release` : `${key} · Double tap to lock`;
      button.textContent = state === "locked" ? `▸ ${key}` : key;
    }
  }
  private readonly layout = (): void => {
    if (!this.focused || !this.phone.matches) {
      this.bar.remove();
      this.host.style.maxHeight = this.originalMaxHeight;
      if (!this.phone.matches) { this.modifiers.reset(); this.paint(); }
    } else {
      if (!this.bar.isConnected) document.body.append(this.bar);
      const bottom = this.viewport ? this.viewport.offsetTop + this.viewport.height : window.innerHeight;
      this.bar.style.left = `${this.viewport?.offsetLeft ?? 0}px`;
      this.bar.style.width = `${this.viewport?.width ?? window.innerWidth}px`;
      this.bar.style.top = `${bottom - this.bar.getBoundingClientRect().height}px`;
      const height = Math.max(0, bottom - this.bar.getBoundingClientRect().height - this.host.getBoundingClientRect().top);
      this.host.style.maxHeight = `${height}px`;
    }
    this.refit();
  };
  dispose(): void {
    this.host.removeEventListener("keydown", this.onKeyDown, true);
    this.host.removeEventListener("keyup", this.onKeyUp, true);
    this.softInput.dispose();
    this.observer.disconnect();
    this.phone.removeEventListener("change", this.layout);
    this.viewport?.removeEventListener("resize", this.layout);
    this.viewport?.removeEventListener("scroll", this.layout);
    window.removeEventListener("resize", this.layout);
    this.host.style.maxHeight = this.originalMaxHeight;
    this.bar.remove();
  }
}
