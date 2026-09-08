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
    for (const modifier of ["Ctrl", "Alt"] as const) if (this.values[modifier] === "once") this.values[modifier] = "off";
    return result;
  }
  input(text: string): string {
    // xterm emits complete escape sequences, control keys and terminal replies.
    // Those are not the next soft-keyboard character; never rewrite their tails.
    if (!text || text.charCodeAt(0) < 32 || text.charCodeAt(0) === 127) return text;
    return Array.from(text, char => this.key(char)).join("");
  }
}

export function keybarPointerDown(event: Pick<Event, "preventDefault">, act: () => void): void {
  event.preventDefault(); // Cancel the browser's button-focus default before acting.
  act();
}

export const KEYBAR_ROWS = [["Ctrl", "Alt", "Esc", "Tab", "^C", "Arrows"], ["More keys", "←", "↑", "↓", "→"]] as const;

export class TerminalKeybar {
  private arrows = false;
  private readonly rows: HTMLElement[] = [];

  private readonly modifiers = new StickyModifiers();
  private readonly bar = document.createElement("div");
  private readonly phone = window.matchMedia("(max-width: 768px)");
  private readonly viewport = window.visualViewport;
  private readonly observer: ResizeObserver;
  private focused = false;
  private physicalInput = false;
  private readonly onKeyDown = (event: KeyboardEvent): void => {
    this.physicalInput = event.key.length === 1 && !event.isComposing && event.keyCode !== 229;
  };
  private readonly onKeyUp = (): void => { this.physicalInput = false; };
  private readonly onSoftInput = (event: InputEvent): void => {
    if (!this.focused || !this.phone.matches || this.physicalInput || event.isComposing ||
      event.inputType !== "insertText" || !event.data) return;
    if (event.type === "beforeinput" && !event.cancelable) return;
    // xterm 5 can retain its keydown flag when a shortcut blurs the textarea
    // before keyup. Claim all soft insertText before xterm, including plain text
    // after a bar key consumes the one-shot. Leave physical keypress and IME
    // commits to xterm; cancelling beforeinput prevents a duplicate input event.
    event.preventDefault();
    event.stopImmediatePropagation();
    this.input(event.data);
  };
  private readonly buttons = new Map<Modifier, HTMLButtonElement>();
  private readonly originalMaxHeight: string;

  constructor(private readonly host: HTMLElement, private readonly input: (data: string) => void,
    private readonly refit: () => void, private readonly applicationCursor: () => boolean) {
    this.originalMaxHeight = host.style.maxHeight;
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
          else this.input(this.modifiers.key(key, this.applicationCursor()));
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
    host.addEventListener("beforeinput", this.onSoftInput as EventListener, true);
    host.addEventListener("input", this.onSoftInput as EventListener, true);
    this.paint();
  }

  setFocused(focused: boolean): void {
    this.focused = focused;
    if (!focused) { this.modifiers.reset(); this.physicalInput = false; this.arrows = false; }
    this.paint();
    this.layout();
  }
  transform(text: string): string {
    const output = this.focused && this.phone.matches ? this.modifiers.input(text) : text;
    this.paint();
    return output;
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
    this.host.removeEventListener("beforeinput", this.onSoftInput as EventListener, true);
    this.host.removeEventListener("input", this.onSoftInput as EventListener, true);
    this.observer.disconnect();
    this.phone.removeEventListener("change", this.layout);
    this.viewport?.removeEventListener("resize", this.layout);
    this.viewport?.removeEventListener("scroll", this.layout);
    window.removeEventListener("resize", this.layout);
    this.host.style.maxHeight = this.originalMaxHeight;
    this.bar.remove();
  }
}
