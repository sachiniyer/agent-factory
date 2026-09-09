import { TerminalSoftInput } from "./terminal-soft-input.js";

const ARROW_SUFFIXES: Record<string, string> = { "←": "D", "↑": "A", "↓": "B", "→": "C" };
const SPECIAL_BYTES: Record<string, string> = { Esc: "\x1b", Tab: "\t", "^C": "\x03" };
export const KEY_BYTES_NAMED_KEYS = Object.freeze([...Object.keys(ARROW_SUFFIXES), ...Object.keys(SPECIAL_BYTES)]);

/** Phone terminal controls and xterm-compatible key encodings. */
export function keyBytes(key: string, ctrl = false, alt = false, applicationCursor = false): string {
  const sequence = userSequence(key);
  if (sequence) return encodeSequence(sequence, (alt ? 2 : 0) | (ctrl ? 4 : 0), key);
  if (ARROW_SUFFIXES[key]) {
    const modifier = 1 + (alt ? 2 : 0) + (ctrl ? 4 : 0);
    // Modified arrows always use CSI, including in application-cursor mode.
    return modifier > 1 ? `\x1b[1;${modifier}${ARROW_SUFFIXES[key]}`
      : `\x1b${applicationCursor ? "O" : "["}${ARROW_SUFFIXES[key]}`;
  }
  // Ctrl+Tab has no legacy byte form: send Tab and consume the one-shot.
  // Esc and ^C already encode control bytes; Alt prefixes all three with ESC.
  if (SPECIAL_BYTES[key]) return (alt ? "\x1b" : "") + SPECIAL_BYTES[key];
  // Xterm represents Backspace as DEL and Ctrl+Backspace as BS.
  if (key === "\x7f") return (alt ? "\x1b" : "") + (ctrl ? "\x08" : key);
  const rawCode = key.charCodeAt(0);
  let text = key;
  if (ctrl && key.length === 1 && rawCode >= 64 && rawCode <= 127) {
    const upperCode = key.toUpperCase().charCodeAt(0);
    if (upperCode >= 64 && upperCode <= 95) text = String.fromCharCode(upperCode & 31);
  }
  return (alt ? "\x1b" : "") + text;
}

/** The complete intended keyBytes key domain: named keys plus one Unicode scalar. */
export function* keyBytesDomain(): Generator<string> {
  yield* KEY_BYTES_NAMED_KEYS;
  for (let codePoint = 0; codePoint <= 0x10ffff; codePoint++) {
    if (codePoint < 0xd800 || codePoint > 0xdfff) yield String.fromCodePoint(codePoint);
  }
}

export const KEYBAR_ROWS = [["Ctrl", "Alt", "Esc", "Tab", "^C", "Arrows"], ["More keys", "←", "↑", "↓", "→"]] as const;

export interface DecodedKeyBytes {
  key: string;
  ctrl: boolean;
  alt: boolean;
  applicationCursor: boolean;
}

interface UserSequence {
  kind: "CSI" | "SS3";
  parameters: string;
  final: string;
}

interface PhysicalKeyInput {
  key: string;
  shiftKey: boolean;
  altKey: boolean;
  ctrlKey: boolean;
  metaKey: boolean;
}

interface UserInputMarker {
  source: "user" | "keybar";
  physical?: PhysicalKeyInput;
}

function userSequence(text: string): UserSequence | undefined {
  if (text.length < 3 || text.charCodeAt(0) !== 27) return undefined;
  const csi = /^\x1b\[([0-9;]*)([A-Za-z~])$/.exec(text);
  if (csi) return { kind: "CSI", parameters: csi[1], final: csi[2] };
  const ss3 = /^\x1bO([\x40-\x7e])$/.exec(text);
  if (ss3) return { kind: "SS3", parameters: "", final: ss3[1] };
  return undefined;
}

function encodeSequence(sequence: UserSequence, modifierBits: number, original: string,
  replaceModifiers = false): string {
  if (!modifierBits) return original;
  if (sequence.kind === "SS3") return `\x1b[1;${modifierBits + 1}${sequence.final}`;
  const parameters = sequence.parameters ? sequence.parameters.split(";") : [];
  parameters[0] ||= "1";
  const encoded = Number(parameters[1] || "1");
  const existingBits = !replaceModifiers && Number.isSafeInteger(encoded) && encoded > 0 ? encoded - 1 : 0;
  parameters[1] = String((existingBits | modifierBits) + 1);
  return `\x1b[${parameters.join(";")}${sequence.final}`;
}

/** Byte-only fallback for user-input paths that have no physical key event. */
export function decodeKeyBytes(text: string): DecodedKeyBytes | undefined {
  let sequenceText = text;
  let prefixedAlt = false;
  let sequence = userSequence(sequenceText);
  if (!sequence && text.charCodeAt(0) === 27) {
    sequenceText = text.slice(1);
    sequence = userSequence(sequenceText);
    prefixedAlt = sequence !== undefined;
  }
  if (sequence) {
    // Xterm emits bare CSI Z for backtab even with Ctrl/Alt held. It has no
    // modifier parameter form, so let the caller consume and pass it through.
    if (sequence.kind === "CSI" && sequence.final === "Z") return undefined;
    const parameters = sequence.parameters ? sequence.parameters.split(";") : [];
    const encoded = Number(parameters[1] || "1");
    const modifierBits = Number.isSafeInteger(encoded) && encoded > 0 ? encoded - 1 : 0;
    return {
      key: sequenceText,
      ctrl: (modifierBits & 4) !== 0,
      alt: prefixedAlt || (modifierBits & 2) !== 0,
      applicationCursor: sequence.kind === "SS3",
    };
  }
  const alt = text.length > 1 && text.charCodeAt(0) === 27;
  const character = alt ? text.slice(1) : text;
  const codePoint = character.codePointAt(0);
  if (codePoint === undefined || character.length !== (codePoint > 0xffff ? 2 : 1)) return undefined;
  if (codePoint <= 31)
    return { key: String.fromCharCode(codePoint + 64), ctrl: true, alt, applicationCursor: false };
  return { key: character, ctrl: false, alt, applicationCursor: false };
}

function physicalKeyBytes(text: string, physical: PhysicalKeyInput, stickyCtrl: boolean,
  stickyAlt: boolean): string | undefined {
  const ctrl = physical.ctrlKey || stickyCtrl;
  const alt = physical.altKey || stickyAlt;
  const modifierBits = (physical.shiftKey ? 1 : 0) | (alt ? 2 : 0) |
    (ctrl ? 4 : 0) | (physical.metaKey ? 8 : 0);
  const sequence = userSequence(text);
  if (sequence) {
    // Xterm emits bare CSI Z for backtab even with Ctrl/Alt held.
    if (sequence.kind === "CSI" && sequence.final === "Z") return undefined;
    // The DOM event is authoritative here. Xterm aliases Alt-only arrows to
    // Ctrl-looking bytes, so the sequence parameter cannot identify the chord.
    return encodeSequence(sequence, modifierBits, text, true);
  }
  const arrow = ({ ArrowLeft: "←", ArrowUp: "↑", ArrowDown: "↓", ArrowRight: "→" } as const)
    [physical.key as "ArrowLeft" | "ArrowUp" | "ArrowDown" | "ArrowRight"];
  // macOS aliases Alt+Left/Right to ESC b/f, so recover arrow identity from key.
  if (arrow) return keyBytes(arrow, ctrl, alt);
  const named = physical.key === "Escape" ? "Esc" : physical.key === "Tab" ? "Tab" : undefined;
  if (named) return keyBytes(named, ctrl, alt);
  if (physical.key === "Backspace") return keyBytes("\x7f", ctrl, alt);
  if (physical.key === "Enter") return keyBytes("\r", ctrl, alt);
  const codePoint = physical.key.codePointAt(0);
  if (codePoint !== undefined && physical.key.length === (codePoint > 0xffff ? 2 : 1))
    return keyBytes(physical.key, ctrl, alt);
  const decoded = decodeKeyBytes(text);
  return decoded ? keyBytes(decoded.key, ctrl, alt, decoded.applicationCursor) : undefined;
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
  input(text: string, source: "terminal" | "user" = "user", physical?: PhysicalKeyInput): string {
    if (source === "terminal") return text;
    const stickyCtrl = this.values.Ctrl !== "off";
    const stickyAlt = this.values.Alt !== "off";
    if (physical && (stickyCtrl || stickyAlt)) {
      const result = physicalKeyBytes(text, physical, stickyCtrl, stickyAlt);
      this.consumeOnce();
      return result ?? text;
    }
    // Soft and deferred textarea input have no physical modifier identity. Their
    // byte shapes are unambiguous here, so the encoder inverse remains a fallback.
    const decoded = decodeKeyBytes(text);
    if (decoded) {
      const result = keyBytes(decoded.key, decoded.ctrl || this.values.Ctrl !== "off",
        decoded.alt || this.values.Alt !== "off", decoded.applicationCursor);
      this.consumeOnce();
      return result;
    }
    // Unrecognized user controls retain the consume-and-pass-through fallback.
    if (!text || text.charCodeAt(0) < 32 || text.charCodeAt(0) === 127) {
      if (text) this.consumeOnce();
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
    if (event.keyCode === 229) this.markDeferredUserInput();
  };
  private readonly onKeyUp = (): void => { this.physicalInput = false; };
  private readonly softInput: TerminalSoftInput;
  private readonly buttons = new Map<Modifier, HTMLButtonElement>();
  private readonly originalMaxHeight: string;
  private userInput: UserInputMarker | undefined;
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
          // Resolve and consume at source. The marker preserves these bytes
          // without sending a keybar emission through the user decoder again.
          else this.sendUserInput(this.modifiers.key(key, this.applicationCursor()), true);
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
    const marker = this.userInput;
    const source = marker || this.takeDeferred229(text) ? "user" : "terminal";
    this.userInput = undefined;
    this.userInputGeneration += 1;
    const output = this.softInput.transform(text, (value, compositionTrailing) =>
      this.focused && this.phone.matches && marker?.source !== "keybar"
        ? this.modifiers.input(value, compositionTrailing ? "user" : source,
          compositionTrailing ? undefined : marker?.physical) : value);
    this.paint();
    return output;
  }
  /** Mark xterm's synchronous onKey emission with its unaliased DOM identity. */
  markUserInput(physical?: PhysicalKeyInput, keybar = false): void {
    const generation = ++this.userInputGeneration;
    this.userInput = {
      source: keybar ? "keybar" : "user",
      physical: physical ? {
        key: physical.key, shiftKey: physical.shiftKey, altKey: physical.altKey,
        ctrlKey: physical.ctrlKey, metaKey: physical.metaKey,
      } : undefined,
    };
    const clear = () => {
      if (this.userInputGeneration === generation) this.userInput = undefined;
    };
    // Synchronous onKey and term.input paths own only the current event turn.
    queueMicrotask(clear);
  }
  /** Match xterm's deferred 229 textarea diff without marking an intervening reply. */
  markDeferredUserInput(): void {
    const before = this.textarea?.value;
    if (before === undefined) return;
    const generation = ++this.deferred229Generation;
    this.deferred229 = { before, generation };
    queueMicrotask(() => setTimeout(() => {
      if (this.deferred229?.generation === generation) this.deferred229 = undefined;
    }, 0));
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
  sendUserInput(data: string, keybar = false): void {
    this.markUserInput(undefined, keybar);
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
