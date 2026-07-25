// The web config-assistant chat pane (#2467, PR2) — the browser counterpart of the
// TUI's config-agent takeover (app/config_agent.go). It opens an overlay hosting a
// live xterm bound to the daemon-owned config assistant, and drives the transport
// contract PR1 settled:
//
//   POST   /v1/config-assistant   spawn-or-reuse   → 200 stream · 409 retry · 503 absent
//   GET    /v1/config-assistant/stream             the PTY WebSocket (AttachTerminal)
//   DELETE /v1/config-assistant   reap on close    (best-effort; a grace reaper backs it)
//
// It owns the whole lifecycle: spawn (with the 409 retry the contract calls for),
// attach the terminal, and on close dispose the terminal and reap. CSP-safe like the
// rest of the client (createElement + addEventListener via h(), no innerHTML).

import { ApiError, reapConfigAssistant, spawnConfigAssistant } from "./api.js";
import { h } from "./modals.js";
import { configAssistantStreamEndpoint } from "./stream_endpoint.js";
import { AttachTerminal, type TerminalStatus } from "./terminal.js";

/** A live config-assistant pane. close() tears the terminal down and reaps. */
export interface ConfigAssistantController {
  close(): void;
}

/** How many times a 409 (create raced a concurrent reap) is retried before giving
 *  up. A DELETE that raced our POST is done by the next attempt, so a fresh POST
 *  spawns anew — a few tries with brief spacing covers a burst without spinning. */
const SPAWN_RETRY_LIMIT = 4;
const SPAWN_RETRY_DELAY_MS = 250;

/** Sentinel thrown to unwind the spawn flow when the pane was closed mid-spawn, so a
 *  late success never attaches a terminal into a removed overlay. */
const CLOSED = Symbol("config-assistant-closed");

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, ms));
}

/**
 * Opens the config-assistant chat overlay, mounting it into `mountHost` (the shell's
 * persistent modal host). Returns a controller whose close() the shell calls to tear
 * it down; the overlay also self-closes on Escape, a backdrop click, or the ×.
 *
 * `token` is the bearer credential for the spawn/stream/reap. `onClosed` fires once,
 * after teardown, so the shell can drop its handle (one assistant pane at a time).
 */
export function openConfigAssistant(opts: {
  token: string;
  mountHost: HTMLElement;
  onClosed?: () => void;
}): ConfigAssistantController {
  const { token, mountHost, onClosed } = opts;

  let closed = false;
  let term: AttachTerminal | null = null;

  // --- overlay DOM ----------------------------------------------------------
  const status = h("span", { class: "af-assistant-status" }, "Starting the assistant…");
  status.setAttribute("role", "status");

  const closeBtn = h("button", { type: "button", class: "af-ghost af-assistant-close" }, "×");
  closeBtn.setAttribute("aria-label", "Close the config assistant");

  const termHost = h("div", { class: "af-assistant-term" });
  const errorLine = h("p", { class: "af-modal-error af-assistant-error", role: "alert" });
  errorLine.hidden = true;

  const card = h(
    "div",
    { class: "af-modal-card af-assistant-card", role: "dialog" },
    h(
      "div",
      { class: "af-assistant-head" },
      h("h2", { class: "af-modal-title" }, "Configure with assistant"),
      status,
      closeBtn,
    ),
    termHost,
    errorLine,
  );
  card.setAttribute("aria-modal", "true");
  card.setAttribute("aria-label", "Config assistant");
  // A click inside the card must not bubble to the backdrop's close handler.
  card.addEventListener("click", (e) => e.stopPropagation());

  const backdrop = h("div", { class: "af-modal-backdrop" }, card);
  backdrop.addEventListener("click", () => close());

  const onKeydown = (e: KeyboardEvent): void => {
    if (e.key === "Escape") {
      e.preventDefault();
      close();
    }
  };
  document.addEventListener("keydown", onKeydown);

  mountHost.append(backdrop);

  // --- status + error surface ----------------------------------------------
  function setStatus(text: string): void {
    status.textContent = text;
  }
  function setError(msg: string): void {
    errorLine.textContent = msg;
    errorLine.hidden = false;
  }

  function terminalStatusText(s: TerminalStatus): string {
    switch (s) {
      case "connecting":
        return "Connecting…";
      case "open":
        return "Connected";
      case "reconnecting":
        return "Reconnecting…";
      case "exited":
        return "Assistant ended.";
    }
  }

  // --- lifecycle ------------------------------------------------------------
  function close(): void {
    if (closed) {
      return;
    }
    closed = true;
    document.removeEventListener("keydown", onKeydown);
    if (term) {
      term.dispose();
      term = null;
    }
    backdrop.remove();
    // Reap the shared assistant. Best-effort: the daemon's last-detach grace reaper
    // is the backstop, and reaping an already-gone assistant is a no-op, so a failure
    // here (including an unreachable daemon) is not worth surfacing on a close.
    void reapConfigAssistant(token).catch(() => {});
    onClosed?.();
  }

  // --- spawn (with the 409 retry the contract calls for) --------------------
  async function spawn(): Promise<void> {
    for (let attempt = 1; ; attempt++) {
      try {
        await spawnConfigAssistant(token);
        return;
      } catch (e) {
        if (closed) {
          throw CLOSED;
        }
        const httpStatus = e instanceof ApiError ? e.status : -1;
        // 409 = the create raced a concurrent reap; retryable — a fresh POST spawns
        // anew once the delete has landed.
        if (httpStatus === 409 && attempt < SPAWN_RETRY_LIMIT) {
          await delay(SPAWN_RETRY_DELAY_MS);
          if (closed) {
            throw CLOSED;
          }
          continue;
        }
        throw e;
      }
    }
  }

  function attach(): void {
    if (closed) {
      return;
    }
    term = new AttachTerminal(termHost, token, configAssistantStreamEndpoint(), {
      onStatus: (s) => {
        if (!closed) {
          setStatus(terminalStatusText(s));
        }
      },
      // The assistant is a single pane; there is no nav/attach mode to keep in sync
      // the way a session split does, so focus changes need no handling here.
      onFocusChange: () => {},
    });
    term.focus();
  }

  void spawn()
    .then(() => attach())
    .catch((e) => {
      if (e === CLOSED || closed) {
        return; // the user closed the pane while spawning; nothing to show
      }
      const httpStatus = e instanceof ApiError ? e.status : -1;
      if (httpStatus === 503) {
        setStatus("Unavailable");
        setError("The config assistant is not available in this daemon build.");
      } else if (httpStatus === 0) {
        setStatus("Offline");
        setError("Could not reach the daemon. Close and try again.");
      } else {
        setStatus("Failed to start");
        setError(e instanceof Error ? e.message : "Could not start the config assistant.");
      }
    });

  return { close };
}
