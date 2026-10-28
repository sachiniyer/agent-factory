package agentproto

// RelayPath is the agent-server route that relays one TCP connection to
// 127.0.0.1:<port> inside the workspace (W5.1, #5571). The port rides the
// ?port= query; the host is never caller-chosen. The websocket carries the
// connection's raw bytes as binary messages, on its own socket so a large
// response cannot head-of-line block a PTY stream.
const RelayPath = "/v1/agent/relay"

// RelayPortQueryParam names the target-port query parameter of RelayPath.
const RelayPortQueryParam = "port"
