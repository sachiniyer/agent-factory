// The web terminal's adjustable, persisted font size (#5500), in one module:
// the two per-device-class values, the read that lands them at attach, the
// step the A−/A+ controls drive, and the debounced save behind them.
//
// The values live in the daemon's [web] config table — NOT in localStorage —
// so they are shared by every browser that attaches to this daemon and they
// survive a page reload and a daemon restart alike. The classes are separate
// because their defaults already were (#5492): phone and desktop keep their
// own size, and stepping one never touches the other.
//
// One module-level holder rather than per-pane state, deliberately: the size
// is a property of the device class, not of a pane, so every terminal in the
// page — splits, the config assistant, the account-login overlay — shows the
// same pitch and steps it together. AttachTerminal subscribes for changes and
// refits through the same OpResize path a breakpoint crossing takes.

import type { ConfigEntry } from "./types.js";

/** The daemon's [web] table and its two leaves, spelled the config way. */
const WEB_ENTRY = "web";
const CONFIG_KEY_PHONE = "web.terminal_font_size_phone";
const CONFIG_KEY_DESKTOP = "web.terminal_font_size_desktop";

/** Same clamp the daemon's validator enforces: a stepped value can never be
 *  refused by the writer it feeds. */
export const TERMINAL_FONT_SIZE_MIN = 4;
export const TERMINAL_FONT_SIZE_MAX = 32;

/** Approved step per press — finer on phone, where a point is a bigger share
 *  of the pitch (#5500). */
export const TERMINAL_FONT_STEP_PHONE = 0.5;
export const TERMINAL_FONT_STEP_DESKTOP = 1;

/** The compiled-in sizes — byte-identical to the config defaults, so a config
 *  that never mentions [web] (an older daemon's manifest has no entry for it)
 *  behaves exactly like today's defaults. */
export const TERMINAL_FONT_SIZE_PHONE_DEFAULT = 6.5;
export const TERMINAL_FONT_SIZE_DESKTOP_DEFAULT = 13;

/** The ONE phone boundary, shared with terminal.ts and the keybar. */
const PHONE_MEDIA_QUERY = "(max-width: 768px)";

/** Quiet period after the last press before the reached size is saved: the
 *  write carries the value the burst SETTLED on rather than every intermediate
 *  one, and index.ts's per-key queue keeps two settled saves in order. */
const PERSIST_DEBOUNCE_MS = 400;

export type DeviceClass = "phone" | "desktop";

const sizes: Record<DeviceClass, number> = {
  phone: TERMINAL_FONT_SIZE_PHONE_DEFAULT,
  desktop: TERMINAL_FONT_SIZE_DESKTOP_DEFAULT,
};

const listeners = new Set<() => void>();

/** The shell's config writer — index.ts installs applyConfigValue, which
 *  serializes same-key saves through createKeyedQueue so presses cannot race
 *  the file. The returned promise settles when the save's whole round trip —
 *  the write AND the manifest refetch it triggers — has completed; a bound save
 *  that resolves early shortens the stale-read guard accordingly. Null before
 *  the app connects; a step with no writer still applies locally (the size is
 *  display state before it is a config value). */
let persist: ((key: string, value: string) => Promise<unknown>) | null = null;

const persistTimers = new Map<DeviceClass, ReturnType<typeof setTimeout>>();

/** Saves that have left the debounce but not yet completed, per class. The
 *  armed timer is only the first half of "a stepped size is newer than anything
 *  the daemon can serve": on a slow link the timer lapses the moment the write
 *  STARTS, while the save sits queued behind another or its echo refetch is
 *  still in the air — and a config read landing in that window carries the
 *  pre-write value. Guarding on the timer alone accepts it and drops the
 *  stepped size back (Codex P2 on #5501): the count covers the write through
 *  completion instead. */
const pendingSaves = new Map<DeviceClass, number>();

/** The font size a terminal on this device class should use right now. */
export function terminalFontSizeFor(phone: boolean): number {
  return phone ? sizes.phone : sizes.desktop;
}

/** Subscribes to size changes from ANY source — a step on either class, or a
 *  config read landing a value another client saved. Returns the unsubscribe;
 *  AttachTerminal calls it from dispose(). */
export function subscribeTerminalFontSizes(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** Installs the save path. index.ts binds it to applyConfigValue so a stepped
 *  size reaches config.toml through the same validated, per-key-serialized
 *  write the config pane uses. The bound save resolves its promise when the
 *  write's whole round trip — POST and the refetch it triggers — is done. */
export function bindTerminalFontPersist(save: (key: string, value: string) => Promise<unknown>): void {
  persist = save;
}

/** Maps a config key back to its device class — null for every key that is not
 *  a font leaf. The shell uses it to route a failed font save to a surface the
 *  user can see, since no manifest row exists for the dotted leaves. */
export function terminalFontClassForKey(key: string): DeviceClass | null {
  if (key === CONFIG_KEY_PHONE) return "phone";
  if (key === CONFIG_KEY_DESKTOP) return "desktop";
  return null;
}

/** Reads the two sizes out of a GetConfig manifest: the `web` entry's compact-
 *  JSON value, per leaf. A missing entry (older daemon), a malformed value, or
 *  a leaf that is not a usable number each fall back to that class's compiled-
 *  in default — never to a guess the renderer would refuse. */
export function terminalFontSizesFromConfig(entries: ConfigEntry[]): Record<DeviceClass, number> {
  const next = { phone: TERMINAL_FONT_SIZE_PHONE_DEFAULT, desktop: TERMINAL_FONT_SIZE_DESKTOP_DEFAULT };
  const entry = entries.find((e) => e.key === WEB_ENTRY);
  if (entry === undefined) {
    return next;
  }
  let table: Record<string, unknown>;
  try {
    const parsed: unknown = JSON.parse(entry.value);
    if (typeof parsed !== "object" || parsed === null) {
      return next;
    }
    table = parsed as Record<string, unknown>;
  } catch {
    return next;
  }
  const leaf = (name: string, fallback: number): number => {
    const value = table[name];
    return typeof value === "number" && Number.isFinite(value) &&
      value >= TERMINAL_FONT_SIZE_MIN && value <= TERMINAL_FONT_SIZE_MAX ? value : fallback;
  };
  return {
    phone: leaf("terminal_font_size_phone", next.phone),
    desktop: leaf("terminal_font_size_desktop", next.desktop),
  };
}

/** Installs freshly read values — the connect-time read and every config
 *  refetch land here. A value identical to the current one notifies nothing,
 *  so the post-save refetch that echoes our own write back is free.
 *
 *  A class with an armed debounce OR a save still queued or in flight is
 *  deliberately SKIPPED: the stepped size is newer than anything the daemon
 *  can serve yet, and dropping it back to the pre-write value — then snapping
 *  forward when the write lands — is exactly the flicker the debounce exists
 *  to prevent. The guard ends only when the last pending save's whole round
 *  trip completes, so the refetch a save triggers cannot reintroduce the
 *  pre-write value while the next queued write is still outstanding. */
export function setTerminalFontSizes(next: Record<DeviceClass, number>): void {
  let changed = false;
  for (const cls of ["phone", "desktop"] as const) {
    if (persistTimers.has(cls) || pendingSaves.has(cls) || next[cls] === sizes[cls]) {
      continue;
    }
    sizes[cls] = next[cls];
    changed = true;
  }
  if (changed) {
    for (const listener of listeners) {
      listener();
    }
  }
}

/** Test seam: drops the sizes AND any armed or in-flight saves, so a step in
 *  one test cannot fire its write in the next. Production never calls this — a
 *  live save must survive anything but the page going away. */
export function resetTerminalFontSizesForTest(): void {
  for (const timer of persistTimers.values()) {
    clearTimeout(timer);
  }
  persistTimers.clear();
  pendingSaves.clear();
  sizes.phone = TERMINAL_FONT_SIZE_PHONE_DEFAULT;
  sizes.desktop = TERMINAL_FONT_SIZE_DESKTOP_DEFAULT;
}

/** One press of an A−/A+ control: step the size for the device class this
 *  viewport is in, clamped to the range the daemon's writer accepts. The change
 *  applies to every subscribed terminal immediately; the save is debounced so
 *  a burst of presses writes once, carrying the settled value. */
export function stepTerminalFontSize(direction: number, phone = currentDeviceIsPhone()): void {
  const cls: DeviceClass = phone ? "phone" : "desktop";
  const step = phone ? TERMINAL_FONT_STEP_PHONE : TERMINAL_FONT_STEP_DESKTOP;
  const next = clampTerminalFontSize(sizes[cls] + Math.sign(direction) * step);
  if (next === sizes[cls]) {
    return;
  }
  sizes[cls] = next;
  for (const listener of listeners) {
    listener();
  }
  schedulePersist(cls);
}

/** The class this viewport is in right now — the same media query the
 *  breakpoint listener watches. No matchMedia (a non-browser test context)
 *  reads as desktop: the phone classes' callers pass the class explicitly. */
function currentDeviceIsPhone(): boolean {
  return typeof window !== "undefined" && typeof window.matchMedia === "function" &&
    window.matchMedia(PHONE_MEDIA_QUERY).matches;
}

function clampTerminalFontSize(size: number): number {
  // Round to centipixels: stepping an already-fractional configured size must
  // not write the float drift (7.5 + 1 → 8.500000000000002) into config.toml.
  return Math.min(TERMINAL_FONT_SIZE_MAX, Math.max(TERMINAL_FONT_SIZE_MIN, Math.round(size * 100) / 100));
}

/** Arms the debounced save for one class. A burst re-arms, so the wire sees
 *  only where it ended; the per-key queue in index.ts orders two bursts. The
 *  dispatched save is then tracked to completion in pendingSaves, so the stale-
 *  read guard outlives the debounce that launched it. */
function schedulePersist(cls: DeviceClass): void {
  const prior = persistTimers.get(cls);
  if (prior !== undefined) {
    clearTimeout(prior);
  }
  persistTimers.set(cls, setTimeout(() => {
    persistTimers.delete(cls);
    const save = persist;
    if (save === null) {
      return;
    }
    pendingSaves.set(cls, (pendingSaves.get(cls) ?? 0) + 1);
    const settle = (): void => {
      const left = (pendingSaves.get(cls) ?? 1) - 1;
      if (left > 0) {
        pendingSaves.set(cls, left);
      } else {
        pendingSaves.delete(cls);
      }
    };
    try {
      // Settle on EITHER arm: a refused write completes the pending window the
      // same as a landed one — the shell owns the failure's visible surface.
      void Promise.resolve(save(configKeyFor(cls), formatTerminalFontSize(sizes[cls]))).then(settle, settle);
    } catch {
      // A synchronously-throwing writer still completes the pending window.
      settle();
    }
  }, PERSIST_DEBOUNCE_MS));
}

function configKeyFor(cls: DeviceClass): string {
  return cls === "phone" ? CONFIG_KEY_PHONE : CONFIG_KEY_DESKTOP;
}

/** The scalar spelling the config writer canonicalizes — plain %g, no trailing
 *  zeros, so the file shows 7.5 rather than 7.50. */
export function formatTerminalFontSize(size: number): string {
  return String(size);
}
