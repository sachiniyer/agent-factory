// Package agentproto holds the neutral wire types for the Phase 2 agent-server
// protocol (#1592): the binary PTY frame codec, the JSON control/event messages,
// and the auth-handshake helpers a bearer token rides on.
//
// It is a leaf, exactly like apiproto. The daemon will import it for the WS PTY
// broker and events plane (Phase 2 PR5), and the TUI/CLI/web clients import it to
// speak the same wire format; so agentproto must depend on NEITHER daemon nor any
// client package, or those importers would form a cycle. It imports only the
// standard library and github.com/coder/websocket.
//
// # Reuse, not re-declaration, of the control plane
//
// The control plane stays the #1029 REST mirror: POST /v1/<Method> with the
// apiproto.Envelope wrapper, whose bodies are the daemon RPC request/response
// structs (daemon.CreateSessionRequest, daemon.KillSessionRequest, …) — those
// already carry json tags for the HTTP server. agentproto deliberately does NOT
// mirror, alias, or re-declare any of them: a second copy could drift from the
// authoritative one, and importing the daemon package here would create the
// import cycle described above. The control action contract therefore has exactly
// one definition, in package daemon, and this package adds only what is genuinely
// new to Phase 2 — the PTY data plane, the resize/exit/detach control frames, the
// events plane, and the auth-readiness seam.
//
// # Multi-writer, no lease
//
// Sachin's decision supersedes the design doc's attach-lease sections (§3-Q3 /
// §4.2): af is single-owner, so every WS subscriber is an equal read-write client
// and the server accepts INPUT/RESIZE from any of them — there is no lease,
// interactive/observer mode, or NACK. The one real multi-client conflict, terminal
// size, is resolved by last-resize-wins plus the authoritative MsgResize echo. A
// lease could be layered on later as purely additive advisory frames without
// reshaping anything here; it is deliberately not built now.
//
// # What is defined here
//
//   - frame.go   — binary PTY frames: PTY_OUT / INPUT / RESIZE opcodes + codec.
//   - message.go — JSON control frames (resize/exit/detach) carried as WS text
//     frames on the PTY stream, and the events-plane Event envelope.
//   - auth.go    — transport-carried bearer-token extraction (Authorization
//     header + ?access_token= query fallback per §4.4). Types and pure helpers
//     only; Phase 2 enforces nothing (the unix-socket peer is trusted, #1029).
//   - wsconn.go  — thin read/write adapters that move the above over a
//     github.com/coder/websocket connection.
package agentproto
