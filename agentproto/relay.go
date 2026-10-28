package agentproto

// RelayPath is the agent-server route that relays one TCP connection to
// 127.0.0.1:<port> inside the workspace (W5.1, #5571). The port rides the
// ?port= query; the host is never caller-chosen. The websocket carries the
// connection's raw bytes as binary messages, on its own socket so a large
// response cannot head-of-line block a PTY stream.
const RelayPath = "/v1/agent/relay"

// RelayPortQueryParam names the target-port query parameter of RelayPath.
const RelayPortQueryParam = "port"

// RelayTargetQueryParam names a server-owned relay target in place of a port.
// The only target is the workspace's editor (W5.4); the caller never supplies a
// path or host, so the relay still reaches nothing the server did not start.
const RelayTargetQueryParam = "target"

// RelayTargetEditor selects the workspace's editor socket.
const RelayTargetEditor = "editor"

// EditorPath is the agent-server route that starts (or reports on) the
// workspace's editor: POST, answered with a state of "ready", "starting" or
// "missing" (no editor binary installed on the host).
const EditorPath = "/v1/agent/editor"

// EditorStopPath ends the workspace's editor (POST): the daemon calls it when the
// session's last VS Code tab closes. It stops only the editor this agent-server
// started for this workspace.
const EditorStopPath = "/v1/agent/editor-stop"

// Editor states answered by EditorPath.
const (
	EditorStateReady    = "ready"
	EditorStateStarting = "starting"
	EditorStateMissing  = "missing"
)

// EditorFeature is the /v1/health token an agent-server advertises when it
// serves EditorPath and the editor relay target.
const EditorFeature = "editor"
