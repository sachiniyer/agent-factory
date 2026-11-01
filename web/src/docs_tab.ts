// The main bundle's side of the Docs tab: it loads the lazy Docs bundle
// (docs_pane.ts and CodeMirror, built into dist/lazy/ by build.mjs) the first
// time a Docs tab opens, and relays proposal.changed and proposal.push events to
// open tabs. Keep
// this module small: everything else about the tab lives in the lazy bundle.

import { errorText } from "./api.js";
import type { ProposalChange, PushStatus } from "./proposal_api.js";

type DocsModule = typeof import("./docs_pane.js");

let docsModule: Promise<DocsModule> | null = null;

/** Loads the Docs bundle once. A failed load is forgotten, so Retry fetches again. */
function loadDocsModule(): Promise<DocsModule> {
  if (!docsModule) {
    // A computed specifier keeps esbuild from inlining the Docs bundle here.
    const url = new URL("./lazy/docs-editor.js", import.meta.url).href;
    docsModule = (import(url) as Promise<DocsModule>).catch((e: unknown) => {
      docsModule = null;
      throw e;
    });
  }
  return docsModule;
}

const listeners = new Set<(change: ProposalChange) => void>();

/** Hands a proposal.changed event to every open Docs tab. */
export function publishProposalChange(change: ProposalChange): void {
  for (const fn of listeners) fn(change);
}

function subscribe(fn: (change: ProposalChange) => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

const pushListeners = new Set<(status: PushStatus) => void>();

/** Hands a proposal.push event to every open Docs tab. */
export function publishProposalPush(status: PushStatus): void {
  for (const fn of pushListeners) fn(status);
}

const resyncListeners = new Set<() => void>();

/** Tells every open Docs tab the event stream reconnected, so it refreshes what
 *  it may have missed. */
export function publishProposalResync(): void {
  for (const fn of resyncListeners) fn();
}

function subscribeResync(fn: () => void): () => void {
  resyncListeners.add(fn);
  return () => resyncListeners.delete(fn);
}

function subscribePush(fn: (status: PushStatus) => void): () => void {
  pushListeners.add(fn);
  return () => pushListeners.delete(fn);
}

export interface DocsTabOptions {
  sessionId: string;
  slug: string;
  token: string;
}

/** Shows the Docs tab for one proposal in `host`. Returns its teardown. */
export function mountDocsTab(host: HTMLElement, opts: DocsTabOptions): () => void {
  let disposed = false;
  let pane: { dispose(): void } | null = null;
  const notice = (text: string, retry: boolean): void => {
    const box = document.createElement("div");
    box.className = "af-docs-notice";
    const p = document.createElement("p");
    p.className = "af-docs-notice-text";
    p.textContent = text;
    box.append(p);
    if (retry) {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "af-ghost";
      btn.textContent = "Retry";
      btn.addEventListener("click", load);
      box.append(btn);
    }
    const wrap = document.createElement("div");
    wrap.className = "af-docs";
    wrap.append(box);
    host.replaceChildren(wrap);
  };
  function load(): void {
    notice("Loading editor…", false);
    loadDocsModule().then(
      (mod) => {
        if (!disposed) pane = new mod.DocsPane(host, { ...opts, subscribe, subscribePush, subscribeResync });
      },
      (e: unknown) => {
        if (!disposed) notice(`Could not load the editor: ${errorText(e)}`, true);
      },
    );
  }
  load();
  return () => {
    disposed = true;
    pane?.dispose();
  };
}
