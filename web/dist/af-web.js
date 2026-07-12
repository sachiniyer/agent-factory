// src/frame.ts
var Op = /* @__PURE__ */ ((Op2) => {
  Op2[Op2["PTYOut"] = 0] = "PTYOut";
  Op2[Op2["Input"] = 1] = "Input";
  Op2[Op2["Resize"] = 2] = "Resize";
  Op2[Op2["Repaint"] = 3] = "Repaint";
  Op2[Op2["Hello"] = 4] = "Hello";
  return Op2;
})(Op || {});
var RESIZE_PAYLOAD_LEN = 4;
var HELLO_PAYLOAD_LEN = 8;
var EMPTY = new Uint8Array(0);
function makeFrame(f) {
  return {
    op: f.op,
    data: f.data ?? EMPTY,
    rows: f.rows ?? 0,
    cols: f.cols ?? 0,
    seq: f.seq ?? 0n
  };
}
function ptyOutFrame(data) {
  return makeFrame({ op: 0 /* PTYOut */, data });
}
function inputFrame(data) {
  return makeFrame({ op: 1 /* Input */, data });
}
function repaintFrame(data) {
  return makeFrame({ op: 3 /* Repaint */, data });
}
function resizeFrame(rows, cols) {
  return makeFrame({ op: 2 /* Resize */, rows, cols });
}
function helloFrame(seq) {
  return makeFrame({ op: 4 /* Hello */, seq });
}
function encode(f) {
  switch (f.op) {
    case 2 /* Resize */: {
      const out = new Uint8Array(1 + RESIZE_PAYLOAD_LEN);
      out[0] = 2 /* Resize */;
      const dv = new DataView(out.buffer);
      dv.setUint16(1, f.rows, false);
      dv.setUint16(3, f.cols, false);
      return out;
    }
    case 4 /* Hello */: {
      const out = new Uint8Array(1 + HELLO_PAYLOAD_LEN);
      out[0] = 4 /* Hello */;
      const dv = new DataView(out.buffer);
      dv.setBigUint64(1, f.seq, false);
      return out;
    }
    default: {
      const out = new Uint8Array(1 + f.data.length);
      out[0] = f.op;
      out.set(f.data, 1);
      return out;
    }
  }
}
function decode(raw) {
  if (raw.length === 0) {
    throw new Error("agentproto: empty binary frame");
  }
  const op = raw[0];
  const body = raw.subarray(1);
  switch (op) {
    case 0 /* PTYOut */:
    case 1 /* Input */:
    case 3 /* Repaint */:
      return makeFrame({ op, data: body.slice() });
    case 2 /* Resize */: {
      if (body.length !== RESIZE_PAYLOAD_LEN) {
        throw new Error(`agentproto: RESIZE frame body is ${body.length} bytes, want ${RESIZE_PAYLOAD_LEN}`);
      }
      const dv = new DataView(body.buffer, body.byteOffset, body.byteLength);
      return makeFrame({ op, rows: dv.getUint16(0, false), cols: dv.getUint16(2, false) });
    }
    case 4 /* Hello */: {
      if (body.length !== HELLO_PAYLOAD_LEN) {
        throw new Error(`agentproto: HELLO frame body is ${body.length} bytes, want ${HELLO_PAYLOAD_LEN}`);
      }
      const dv = new DataView(body.buffer, body.byteOffset, body.byteLength);
      return makeFrame({ op, seq: dv.getBigUint64(0, false) });
    }
    default:
      throw new Error(`agentproto: unknown opcode 0x${op.toString(16).padStart(2, "0")}`);
  }
}
function opName(op) {
  switch (op) {
    case 0 /* PTYOut */:
      return "PTY_OUT";
    case 1 /* Input */:
      return "INPUT";
    case 2 /* Resize */:
      return "RESIZE";
    case 3 /* Repaint */:
      return "REPAINT";
    case 4 /* Hello */:
      return "HELLO";
    default:
      return `Opcode(0x${op.toString(16).padStart(2, "0")})`;
  }
}
export {
  HELLO_PAYLOAD_LEN,
  Op,
  RESIZE_PAYLOAD_LEN,
  decode,
  encode,
  helloFrame,
  inputFrame,
  opName,
  ptyOutFrame,
  repaintFrame,
  resizeFrame
};
