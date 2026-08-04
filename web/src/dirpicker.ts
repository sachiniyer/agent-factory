// The Add-project directory picker (#2788).
//
// "Type an absolute path" is unusable from a phone, or from any browser that is
// not sitting on the daemon host: there is no tab-completion, nothing shows what
// exists, and a typo is only discovered after the round trip. This is the
// minimal browser that fixes that — directories only, one line per entry, click
// to descend, an up affordance, the current path as a header, and a git-repo
// mark on the only entries that can actually become a project.
//
// It is an AFFORDANCE, not a replacement: the modal keeps its free-text path
// field, which stays the single input the submit reads. Picking a repo here
// fills that field. Anyone who knows the path still pastes it, and a path this
// browser cannot reach stays reachable.
//
// THE INVARIANT, split out as pure functions below so it is unit-testable
// without a DOM: a navigation that FAILS does not move the picker and never
// produces an empty list. An unreadable directory and an empty one are opposite
// facts that render identically once a refusal is flattened into `entries: []`
// — the fabricated-negative shape this repo keeps re-learning — so a failure
// keeps the listing the user is actually standing in and shows the daemon's
// message above it.

import type { DirectoryEntry, DirectoryListing } from "./api.js";
import { icon } from "./icon.js";
import { h } from "./dom.js";

// --- state (pure, DOM-free) ------------------------------------------------

/** What the picker is showing: where it is, what went wrong last, and whether a
 *  navigation is in flight. `listing` is null only before the first successful
 *  load — a failure never nulls it. */
export interface PickerState {
  listing: DirectoryListing | null;
  error: string | null;
  loading: boolean;
}

export const INITIAL_PICKER_STATE: PickerState = { listing: null, error: null, loading: false };

/** A navigation started: keep everything on screen, mark it in flight. The old
 *  error stays until the outcome is known, so the message does not blink away
 *  and back on a retry of the same refused directory. */
export function pickerLoading(prev: PickerState): PickerState {
  return { ...prev, loading: true };
}

/** A navigation succeeded: the picker MOVES, and the previous error is gone
 *  because it described a directory the user is no longer trying to enter. */
export function pickerLoaded(listing: DirectoryListing): PickerState {
  return { listing, error: null, loading: false };
}

/** A navigation FAILED: the picker does not move.
 *
 *  Keeping `prev.listing` is the whole point. The alternative — clearing it and
 *  rendering an empty list next to an error — is the shape where a permission
 *  denial and an empty directory become indistinguishable at a glance. A failed
 *  descent simply did not happen, so the user stays where they were, with the
 *  daemon's reason above the list they can still navigate. */
export function pickerFailed(prev: PickerState, message: string): PickerState {
  return { listing: prev.listing, error: message, loading: false };
}

/** The muted note on one row: what this entry is, in the picker's terms. Empty
 *  for an ordinary directory — the absence of "git repo" is what says "navigable
 *  but not a target". */
export function entryNote(entry: DirectoryEntry): string {
  const parts: string[] = [];
  if (entry.is_repo) {
    parts.push("git repo");
  }
  if (entry.is_symlink) {
    parts.push("link");
  }
  return parts.join(" · ");
}

/** The line under a capped listing, or "" when nothing was dropped. Never
 *  silent: a truncated list that looked complete is the same wrong answer as an
 *  empty one. */
export function truncationNote(listing: DirectoryListing): string {
  if (!listing.truncated) {
    return "";
  }
  return `Showing the first ${listing.entries.length} directories — type the path below to reach one that is not listed.`;
}

// --- persistence -----------------------------------------------------------

/** localStorage key for the directory the picker last browsed, so re-opening
 *  Add project resumes where the user was rather than at $HOME every time. */
const LAST_DIR_KEY = "af.addproject.dir";

/** The remembered starting directory, or "" for "let the daemon pick" (its
 *  home). Never throws — persistence is a convenience. */
export function loadLastBrowsedDir(): string {
  try {
    return localStorage.getItem(LAST_DIR_KEY) ?? "";
  } catch {
    return "";
  }
}

/** Remembers the browsed directory. Best-effort, like every other web
 *  preference (private mode / disabled storage just loses the convenience). */
export function persistLastBrowsedDir(path: string): void {
  try {
    localStorage.setItem(LAST_DIR_KEY, path);
  } catch {
    // no-op: persistence is best-effort
  }
}

// --- the component ---------------------------------------------------------

/** A mounted picker. `el` goes in the modal body; `start()` kicks off the first
 *  load (deferred so the caller controls when the request goes out). */
export interface DirectoryPickerHandle {
  el: HTMLElement;
  start(): void;
}

/** Builds the picker.
 *
 *  `load` issues the daemon read (index.ts binds the token). `onSelect` fires
 *  with a canonical repo path the user chose — the modal writes it into its path
 *  field rather than submitting, so what gets registered is always the string
 *  the user can see and edit.
 *
 *  Every button here is `type="button"`: the picker is mounted INSIDE the
 *  modal's form (modals.ts asForm re-parents the card's children), where a
 *  default-type button would submit the Add-project form on every descent. */
export function directoryPicker(callbacks: {
  load: (path: string) => Promise<DirectoryListing>;
  onSelect: (path: string) => void;
  errorText: (e: unknown) => string;
}): DirectoryPickerHandle {
  let state: PickerState = INITIAL_PICKER_STATE;
  // Monotonic navigation token: only the newest request may commit. Without it a
  // slow listing of a big directory can land after a later one and silently
  // reposition the picker under the user.
  let nav = 0;

  const upBtn = h("button", { type: "button", class: "af-ghost af-dirpicker-up" }, "Up");
  upBtn.setAttribute("aria-label", "Go to the parent directory");
  upBtn.disabled = true;

  const pathLabel = h("span", { class: "af-dirpicker-path" }, "Loading…");

  // The way back after ascending. The daemon's home is a DAEMON-HOST fact the
  // browser cannot compute, which is why the listing carries it; without this the
  // only route back from "/" is descending by hand.
  const homeBtn = h("button", { type: "button", class: "af-ghost af-dirpicker-home" }, "Home");
  homeBtn.setAttribute("aria-label", "Go to the daemon host's home directory");
  homeBtn.hidden = true;

  const useHereBtn = h("button", { type: "button", class: "af-ghost af-dirpicker-use-here" }, "Use this");
  useHereBtn.hidden = true;

  const head = h("div", { class: "af-dirpicker-head" }, upBtn, pathLabel, homeBtn, useHereBtn);

  const errorLine = h("p", { class: "af-dirpicker-error", role: "alert" });
  errorLine.hidden = true;

  const list = h("div", { class: "af-dirpicker-list" });
  list.setAttribute("role", "list");

  // A sibling of the list, not a child: the list is role="list", whose children
  // must be list items.
  const emptyLine = h("p", { class: "af-dirpicker-empty" }, "No subdirectories here.");
  emptyLine.hidden = true;

  const note = h("p", { class: "af-dirpicker-note" });

  const el = h("div", { class: "af-dirpicker" }, head, errorLine, list, emptyLine, note);

  // The path whose rows are currently in the DOM, so a rebuild that does not move
  // the picker can restore its scroll position. Rebuilding a scrollable container
  // with replaceChildren silently drops scrollTop (#1894), and the case that
  // matters is precisely the one this component is careful about: a FAILED
  // navigation leaves the user where they were, and yanking them to the top of a
  // list they had scrolled is a second, gratuitous loss of place.
  let renderedPath: string | null = null;

  function navigate(path: string, fallbackToHome = false): void {
    const ticket = ++nav;
    state = pickerLoading(state);
    render();
    void callbacks
      .load(path)
      .then((listing) => {
        if (ticket !== nav) {
          return;
        }
        state = pickerLoaded(listing);
        persistLastBrowsedDir(listing.path);
        render();
      })
      .catch((e: unknown) => {
        if (ticket !== nav) {
          return;
        }
        // Only the REMEMBERED starting directory falls back: it can name a path
        // that has since moved, or one from a different daemon host, and opening
        // the picker onto an error the user did not ask for is a bad first
        // screen. A navigation the user actually clicked never falls back — that
        // would silently move them somewhere they did not choose.
        if (fallbackToHome) {
          navigate("");
          return;
        }
        state = pickerFailed(state, callbacks.errorText(e));
        render();
      });
  }

  function render(): void {
    const { listing, error, loading } = state;

    pathLabel.textContent = listing ? listing.path : loading ? "Loading…" : "";
    pathLabel.title = listing?.path ?? "";

    upBtn.disabled = loading || !listing || listing.parent === "";
    // Hidden where it would be a no-op (already home) or a lie (the daemon could
    // not resolve a home at all), rather than shown-and-dead.
    homeBtn.hidden = !listing || listing.home === "" || listing.home === listing.path;
    homeBtn.disabled = loading;
    useHereBtn.hidden = !listing?.is_repo;
    if (listing?.is_repo) {
      useHereBtn.disabled = loading;
      useHereBtn.title = `Use ${listing.path} as the project`;
    }

    if (error) {
      errorLine.textContent = error;
      errorLine.hidden = false;
    } else {
      errorLine.textContent = "";
      errorLine.hidden = true;
    }

    const scrollTop = list.scrollTop;
    list.replaceChildren();
    if (listing) {
      for (const entry of listing.entries) {
        list.append(row(entry, loading));
      }
    }
    if (listing && listing.path === renderedPath) {
      list.scrollTop = scrollTop;
    }
    renderedPath = listing?.path ?? null;

    // The empty state is stated only when the daemon actually answered with an
    // empty directory. While an error is showing, the list belongs to wherever
    // the user still is — calling that "nothing here" would be the lie.
    emptyLine.hidden = !listing || listing.entries.length > 0 || error !== null;
    note.textContent = listing ? truncationNote(listing) : "";
  }

  function row(entry: DirectoryEntry, loading: boolean): HTMLElement {
    const label = h(
      "span",
      { class: "af-dirpicker-name" },
      icon(entry.is_repo ? "folder-git" : "folder"),
      h("span", { class: "af-dirpicker-text" }, entry.name),
    );
    const meta = entryNote(entry);
    const open = h("button", { type: "button", class: "af-dirpicker-item" }, label);
    if (meta) {
      open.append(h("span", { class: "af-dirpicker-meta" }, meta));
    }
    open.title = entry.path;
    open.setAttribute("aria-label", `Open ${entry.path}`);
    open.disabled = loading;
    open.addEventListener("click", (e) => {
      e.stopPropagation();
      navigate(entry.path);
    });

    const item = h("div", { class: "af-dirpicker-row" }, open);
    item.setAttribute("role", "listitem");
    // The select affordance exists ONLY on a checkout: a plain directory stays
    // navigable but is visibly not a target, which is the honest rendering of
    // what RegisterProject would do with it.
    if (entry.is_repo) {
      item.classList.add("af-dirpicker-row-repo");
      const use = h("button", { type: "button", class: "af-ghost af-dirpicker-use" }, "Use");
      use.title = `Use ${entry.path} as the project`;
      use.setAttribute("aria-label", `Use ${entry.path} as the project`);
      use.disabled = loading;
      use.addEventListener("click", (e) => {
        e.stopPropagation();
        callbacks.onSelect(entry.path);
      });
      item.append(use);
    }
    return item;
  }

  upBtn.addEventListener("click", (e) => {
    e.stopPropagation();
    const parent = state.listing?.parent;
    if (parent) {
      navigate(parent);
    }
  });
  homeBtn.addEventListener("click", (e) => {
    e.stopPropagation();
    const home = state.listing?.home;
    if (home) {
      navigate(home);
    }
  });
  useHereBtn.addEventListener("click", (e) => {
    e.stopPropagation();
    const path = state.listing?.path;
    if (path) {
      callbacks.onSelect(path);
    }
  });

  return {
    el,
    start() {
      const remembered = loadLastBrowsedDir();
      navigate(remembered, remembered !== "");
    },
  };
}
