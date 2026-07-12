// A faithful, byte-for-byte port of the Go PTY frame codec (agentproto/frame.go).
// The browser web client (#1592 Phase 5) is a second thin client of the same
// WS PTY protocol the TUI/apiclient speak, so it must encode/decode frames
// identically to the daemon. This is the one place the browser reimplements Go, so
// it is pinned to the Go source of truth by a golden-vector test (frame.test.ts)
// that validates both codecs against the SAME fixture (agentproto/testdata/
// frame_vectors.json). Keep this in lockstep with frame.go; do NOT renumber ops.

/** Opcode is the first byte of a binary WS frame on the PTY stream. */
export enum Op {
  /** server → client: verbatim PTY output bytes. */
  PTYOut = 0x00,
  /** client → server: raw key bytes (multi-writer, accepted from any client). */
  Input = 0x01,
  /** client → server: a rows,cols uint16 pair (last-resize-wins). */
  Resize = 0x02,
  /** server → client: one-shot fresh-subscriber repaint; rendered but NOT counted
   *  toward the replay cursor. */
  Repaint = 0x03,
  /** server → client: the subscription's starting seq as a big-endian uint64, sent
   *  as the FIRST frame so a browser — which cannot read the X-Af-Stream-Seq
   *  handshake header — learns its absolute cursor to seed `?since` replay
   *  (#1592 Phase 5 PR1, design §4.3). NOT counted toward the replay cursor. */
  Hello = 0x04,
}

/** The fixed body size of a RESIZE frame: two big-endian uint16s (rows, cols). */
export const RESIZE_PAYLOAD_LEN = 4;
/** The fixed body size of a HELLO frame: one big-endian uint64 (start seq). */
export const HELLO_PAYLOAD_LEN = 8;

/**
 * A decoded binary PTY frame. Mirrors Go's agentproto.Frame: `data` carries the
 * payload for PTYOut/Input/Repaint; `rows`/`cols` carry a Resize; `seq` carries a
 * Hello. Unused fields hold their zero values (empty data, 0, 0n) so a decoded
 * frame compares cleanly against a constructed one.
 */
export interface Frame {
  op: Op;
  data: Uint8Array;
  rows: number;
  cols: number;
  seq: bigint;
}

const EMPTY = new Uint8Array(0);

function makeFrame(f: { op: Op; data?: Uint8Array; rows?: number; cols?: number; seq?: bigint }): Frame {
  return {
    op: f.op,
    data: f.data ?? EMPTY,
    rows: f.rows ?? 0,
    cols: f.cols ?? 0,
    seq: f.seq ?? 0n,
  };
}

/** Wraps verbatim PTY output bytes (server → client). */
export function ptyOutFrame(data: Uint8Array): Frame {
  return makeFrame({ op: Op.PTYOut, data });
}

/** Wraps raw key bytes (client → server). */
export function inputFrame(data: Uint8Array): Frame {
  return makeFrame({ op: Op.Input, data });
}

/** Wraps a one-shot screen repaint (server → client); rendered like PTYOut but NOT
 *  counted toward the replay cursor. */
export function repaintFrame(data: Uint8Array): Frame {
  return makeFrame({ op: Op.Repaint, data });
}

/** Wraps a terminal size (client → server; last-resize-wins). */
export function resizeFrame(rows: number, cols: number): Frame {
  return makeFrame({ op: Op.Resize, rows, cols });
}

/** Wraps a subscription's starting seq (server → client), the in-band cursor seed. */
export function helloFrame(seq: bigint): Frame {
  return makeFrame({ op: Op.Hello, seq });
}

/** Serializes a frame to its opcode-prefixed wire form (byte-identical to Go's
 *  Frame.Encode). */
export function encode(f: Frame): Uint8Array {
  switch (f.op) {
    case Op.Resize: {
      const out = new Uint8Array(1 + RESIZE_PAYLOAD_LEN);
      out[0] = Op.Resize;
      const dv = new DataView(out.buffer);
      dv.setUint16(1, f.rows, false); // big-endian
      dv.setUint16(3, f.cols, false);
      return out;
    }
    case Op.Hello: {
      const out = new Uint8Array(1 + HELLO_PAYLOAD_LEN);
      out[0] = Op.Hello;
      const dv = new DataView(out.buffer);
      dv.setBigUint64(1, f.seq, false); // big-endian
      return out;
    }
    default: {
      // PTYOut / Input / Repaint (and any op): opcode byte then the raw payload.
      const out = new Uint8Array(1 + f.data.length);
      out[0] = f.op;
      out.set(f.data, 1);
      return out;
    }
  }
}

/**
 * Parses an opcode-prefixed binary frame (the inverse of encode; mirrors Go's
 * DecodeFrame including its error behavior). Throws on an empty frame, an unknown
 * opcode, or a malformed RESIZE/HELLO body.
 */
export function decode(raw: Uint8Array): Frame {
  if (raw.length === 0) {
    throw new Error("agentproto: empty binary frame");
  }
  const op = raw[0] as Op;
  const body = raw.subarray(1);
  switch (op) {
    case Op.PTYOut:
    case Op.Input:
    case Op.Repaint:
      // Copy so the returned frame does not alias the caller's buffer.
      return makeFrame({ op, data: body.slice() });
    case Op.Resize: {
      if (body.length !== RESIZE_PAYLOAD_LEN) {
        throw new Error(`agentproto: RESIZE frame body is ${body.length} bytes, want ${RESIZE_PAYLOAD_LEN}`);
      }
      const dv = new DataView(body.buffer, body.byteOffset, body.byteLength);
      return makeFrame({ op, rows: dv.getUint16(0, false), cols: dv.getUint16(2, false) });
    }
    case Op.Hello: {
      if (body.length !== HELLO_PAYLOAD_LEN) {
        throw new Error(`agentproto: HELLO frame body is ${body.length} bytes, want ${HELLO_PAYLOAD_LEN}`);
      }
      const dv = new DataView(body.buffer, body.byteOffset, body.byteLength);
      return makeFrame({ op, seq: dv.getBigUint64(0, false) });
    }
    default:
      throw new Error(`agentproto: unknown opcode 0x${(op as number).toString(16).padStart(2, "0")}`);
  }
}

/** Renders an opcode for diagnostics (mirrors Go's Opcode.String). */
export function opName(op: Op): string {
  switch (op) {
    case Op.PTYOut:
      return "PTY_OUT";
    case Op.Input:
      return "INPUT";
    case Op.Resize:
      return "RESIZE";
    case Op.Repaint:
      return "REPAINT";
    case Op.Hello:
      return "HELLO";
    default:
      return `Opcode(0x${(op as number).toString(16).padStart(2, "0")})`;
  }
}
