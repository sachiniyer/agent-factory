// The channel between the Docs pane (the parent window) and the mermaid
// render frame (#6669). The frame is sandboxed WITHOUT allow-same-origin —
// an opaque origin — so window.postMessage is the only thing that reaches it,
// and it is used exactly twice: once by the frame to announce itself, once by
// the parent to hand over a MessagePort. Everything else travels the port,
// which is bound to the DOCUMENT that announced itself, not to the frame's
// WindowProxy: a port transferred to a document cannot be claimed by whatever
// replaces that document if the frame navigates itself, and a reply arriving
// on a port can only have come from the port's holder. A WindowProxy survives
// navigation, so event.source cannot tell the first document from its
// replacement — the port is what closes that gap.
//
//   frame → parent  window postMessage {t:"af-mmd-ready", hello}   announce
//   parent → frame  window postMessage {t:"af-mmd-port"} + [port]  handoff
//   frame → parent  port             {t:"af-mmd-hello", hello}     bind echo
//   parent → frame  port             {t:"af-mmd-render", id, source, theme}
//   frame → parent  port             {t:"af-mmd-rendered", id, ok, svg|error}
//
// `hello` is a nonce the frame mints at module eval and echoes back over the
// port. The parent posts renders only after the echo matches — so a message
// that lands in a document that never saw the ready announce (a navigation
// replacement; the nonce was inside the first document's announce, which the
// replacement could not observe) is never followed by any diagram source.
//
// Every payload is a JSON STRING: no objects, no functions, no DOM — the only
// thing that ever leaves the frame is text, and the only thing that ever
// enters it is the diagram it is about to render anyway.
//
// Decoders validate the whole shape and return undefined for anything off it;
// the listener treats that as "not for me" and ignores the message. The two
// ends are bundled into separate builds (md_mermaid.ts lands in
// docs-editor.js, mermaid_chunk.ts inline in mermaid-frame.html), which is
// why the protocol lives in its own leaf module both sides import.

export const MSG_READY = "af-mmd-ready" as const;
export const MSG_PORT = "af-mmd-port" as const;
export const MSG_HELLO = "af-mmd-hello" as const;
export const MSG_RENDER = "af-mmd-render" as const;
export const MSG_RENDERED = "af-mmd-rendered" as const;

export interface RenderRequest {
  t: typeof MSG_RENDER;
  id: string;
  source: string;
  theme: Record<string, string>;
}

export type FrameReply =
  | { t: typeof MSG_READY; hello: string }
  | { t: typeof MSG_HELLO; hello: string }
  | { t: typeof MSG_RENDERED; id: string; ok: true; svg: string }
  | { t: typeof MSG_RENDERED; id: string; ok: false; error: string };

function isRecord(m: unknown): m is Record<string, unknown> {
  return typeof m === "object" && m !== null && !Array.isArray(m);
}

function parse(data: unknown): Record<string, unknown> | undefined {
  // The channel carries strings only — an object, however well-shaped, is not
  // the protocol: a structured-clone payload could smuggle capabilities a
  // string cannot.
  if (typeof data !== "string") return undefined;
  let m: unknown;
  try {
    m = JSON.parse(data);
  } catch {
    return undefined;
  }
  return isRecord(m) ? m : undefined;
}

/** Parent → frame: the one request the protocol has. Travels the port. */
export function encodeRenderRequest(id: string, source: string, theme: Record<string, string>): string {
  return JSON.stringify({ t: MSG_RENDER, id, source, theme });
}

/** Frame side: decode a request, or undefined for anything off-shape. The
 *  theme is rebuilt as a fresh null-prototype string record so the rendered
 *  config is pure data whatever the source JSON carried. No source check —
 *  on a port, holding the port IS the identity. */
export function decodeRenderRequest(data: unknown): RenderRequest | undefined {
  const m = parse(data);
  if (!m || m.t !== MSG_RENDER) return undefined;
  if (typeof m.id !== "string" || typeof m.source !== "string" || !isRecord(m.theme)) return undefined;
  const theme: Record<string, string> = Object.create(null) as Record<string, string>;
  for (const [k, v] of Object.entries(m.theme)) {
    if (typeof v !== "string") return undefined;
    theme[k] = v;
  }
  return { t: MSG_RENDER, id: m.id, source: m.source, theme };
}

export function encodeFrameReply(reply: FrameReply): string {
  return JSON.stringify(reply);
}

/** Parent side: decode a frame-originated message — the ready announce on the
 *  window channel, or a hello/rendered on the port. Extra unknown fields are
 *  tolerated — the protocol is the required fields, and a sender that can add
 *  fields is already inside the frame. */
export function decodeFrameReply(data: unknown): FrameReply | undefined {
  const m = parse(data);
  if (!m) return undefined;
  if (m.t === MSG_READY && typeof m.hello === "string" && m.hello !== "") {
    return { t: MSG_READY, hello: m.hello };
  }
  if (m.t === MSG_HELLO && typeof m.hello === "string" && m.hello !== "") {
    return { t: MSG_HELLO, hello: m.hello };
  }
  if (m.t !== MSG_RENDERED || typeof m.id !== "string") return undefined;
  if (m.ok === true && typeof m.svg === "string") return { t: MSG_RENDERED, id: m.id, ok: true, svg: m.svg };
  if (m.ok === false && typeof m.error === "string") return { t: MSG_RENDERED, id: m.id, ok: false, error: m.error };
  return undefined;
}

/** Parent side, window-channel gate: the ready announce counts only when it
 *  came from the render frame's own window AND decodes — the source check
 *  lives inside the function the listener calls, so the gate cannot be
 *  dropped at a call site without the protocol tests failing. `expected` is
 *  the frame's contentWindow (a WindowProxy: identity is per element and
 *  cannot be spoofed by a foreign sender — the DOCUMENT inside it is bound
 *  separately, by the hello echo over the port). */
export function frameReplyFor(e: { source: unknown; data: unknown }, expected: unknown): FrameReply | undefined {
  return e.source === expected ? decodeFrameReply(e.data) : undefined;
}

/** Parent → frame: the port handoff — the ONLY other message the window
 *  channel carries. The transferred MessagePort rides in event.ports, not
 *  the payload. */
export function encodePortGrant(): string {
  return JSON.stringify({ t: MSG_PORT });
}

/** Frame side, handoff gate: the grant counts only when it came from the
 *  parent window, decodes as the grant, and carries exactly one port —
 *  mirrored with frameReplyFor so the gate cannot be dropped at a call site.
 *  `expected` is window.parent in the frame. */
export function portGrantFor(
  e: { source: unknown; data: unknown; ports: readonly unknown[] },
  expected: unknown,
): MessagePort | undefined {
  if (e.source !== expected) return undefined;
  const m = parse(e.data);
  if (!m || m.t !== MSG_PORT) return undefined;
  return e.ports.length === 1 ? (e.ports[0] as MessagePort) : undefined;
}
