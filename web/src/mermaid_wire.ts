// The postMessage wire between the Docs pane (the parent window) and the
// mermaid render frame (#6669). The frame is sandboxed WITHOUT
// allow-same-origin — an opaque origin — so postMessage is the only channel
// that crosses in either direction, and every message is a JSON STRING:
//
//   parent → frame   {t:"af-mmd-render", id, source, theme}   one render
//   frame → parent   {t:"af-mmd-rendered", id, ok, svg|error} its reply
//   frame → parent   {t:"af-mmd-ready"}                       announce at load
//
// No objects, no functions, no DOM — the only thing that ever leaves the
// frame is text, and the only thing that ever enters it is the diagram it is
// about to render anyway.
//
// Decoders validate the whole shape and return undefined for anything off it;
// the listener treats that as "not for me" and ignores the message. The two
// ends are bundled into separate builds (md_mermaid.ts lands in
// docs-editor.js, mermaid_chunk.ts in mermaid.js), which is why the protocol
// lives in its own leaf module both sides import.

export const MSG_READY = "af-mmd-ready" as const;
export const MSG_RENDER = "af-mmd-render" as const;
export const MSG_RENDERED = "af-mmd-rendered" as const;

export interface RenderRequest {
  t: typeof MSG_RENDER;
  id: string;
  source: string;
  theme: Record<string, string>;
}

export type FrameReply =
  | { t: typeof MSG_READY }
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

/** Parent → frame: the one request the protocol has. */
export function encodeRenderRequest(id: string, source: string, theme: Record<string, string>): string {
  return JSON.stringify({ t: MSG_RENDER, id, source, theme });
}

/** Frame side: decode a request, or undefined for anything off-shape. The
 *  theme is rebuilt as a fresh null-prototype string record so the rendered
 *  config is pure data whatever the source JSON carried. */
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

/** Parent side: decode a reply, or undefined for anything off-shape. Extra
 *  unknown fields are tolerated — the protocol is the required fields, and a
 *  sender that can add fields is already inside the frame. */
export function decodeFrameReply(data: unknown): FrameReply | undefined {
  const m = parse(data);
  if (!m) return undefined;
  if (m.t === MSG_READY) return { t: MSG_READY };
  if (m.t !== MSG_RENDERED || typeof m.id !== "string") return undefined;
  if (m.ok === true && typeof m.svg === "string") return { t: MSG_RENDERED, id: m.id, ok: true, svg: m.svg };
  if (m.ok === false && typeof m.error === "string") return { t: MSG_RENDERED, id: m.id, ok: false, error: m.error };
  return undefined;
}
