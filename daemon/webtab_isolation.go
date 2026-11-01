package daemon

import (
	"strings"

	"github.com/sachiniyer/agent-factory/session"
)

// Workspace content never runs with the control origin's authority.
//
// The /v1/webtab/ mirror serves a session's dev server or editor on the SAME origin
// as the web UI, which holds the bearer token in localStorage and passes every
// same-origin check the daemon makes (#6443's cross-site guard admits it as its
// own). So anything a workspace serves there — a dev server's page, a file from the
// worktree, an editor relayed from a sandbox — must not execute script as that
// origin. Two rules, both judged on the mirror only (route.previewOrigin is false);
// the per-tab preview origin and the editor-origin ports are separate origins by
// construction and keep their own posture.
//
//  1. Every mirrored WEB-tab response carries a CSP sandbox (mirrorSandboxCSP),
//     the same flags the web client puts on the mirror's iframe and no
//     allow-same-origin. Framed, that changes nothing: the iframe was already
//     opaque. Opened top-level — the pane's "Open", a copied link, a bookmark —
//     the page used to run unsandboxed as the control origin, able to read the
//     token and drive the API. Now a top-level mirror document is opaque too, so
//     it behaves exactly as it does in the frame. A response's own CSP is kept;
//     policies intersect, so this can only narrow it. CSP sandbox governs
//     documents only, so scripts, styles and images the app loads are unaffected.
//
//  2. A RELAYED editor — the VS Code tab of an off-box workspace (docker, SSH,
//     remote-hook, sandbox), whose bytes come from the workspace itself — is never
//     served on the mirror. VS Code cannot run under an opaque origin, so rule 1
//     cannot contain it, and a workspace that controls the editor's HTML would run
//     script as the control origin: a sandbox escape to the daemon host. It is
//     served only on its own origin — the preview listener's per-session
//     *.localhost name, or an editor-origin port — and the mirror answers with a
//     notice naming how to turn one on.
//
// A LOCAL editor (code-server or openvscode-server spawned by this daemon on this
// machine, for a local-worktree session) stays on the mirror when no editor origin
// is available, deliberately: its workbench is software from the daemon host, not
// workspace content, and a local workspace already runs with the operator's own
// authority (same user, the daemon's unix socket, its token file), so the workbench
// crosses no boundary the workspace does not already hold. Moving it would break
// the default local editor, which has no other origin unless the operator enabled
// network.preview_listen_addr.
//
//  3. What an editor serves that it did not write is sandboxed on EVERY route that
//     serves an editor — the mirror, the preview listener's editor origin and an
//     editor-origin port alike — by path (editorServesWorkspaceContent):
//     code-server's port proxy and VS Code's worktree-file route. On the mirror the
//     reason is the control origin; on an editor's own origin it is the editor
//     itself: a page at the editor's origin is same-origin with its WebSocket,
//     which is a terminal in that workspace, so a dev server reached through
//     /proxy/<port>/ must not run there as a real origin either.

// mirrorSandboxCSP is the opaque-origin sandbox for mirrored workspace documents.
// Its flags must equal the web client's mirror iframe sandbox
// (web/src/tabaddr.ts webSandbox(false, false)), so a page behaves the same framed
// and top-level; TestMirrorSandboxMatchesTheClientsFrameSandbox and its TS twin pin
// both to this literal.
const mirrorSandboxCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals"

// workspaceSandboxPolicy is the CSP the proxy adds to a response, or "" for none:
// rule 1 for a web tab on the mirror (a web tab's own preview origin is separate
// from everything else by design and keeps a real origin), and rule 3 for an
// editor's workspace-content routes on any route.
func workspaceSandboxPolicy(route webTabRoute, kind session.TabKind) string {
	switch kind {
	case session.TabKindWeb:
		if !route.previewOrigin {
			return mirrorSandboxCSP
		}
	case session.TabKindVSCode:
		if editorServesWorkspaceContent(route.rest) {
			return mirrorSandboxCSP
		}
	}
	return ""
}

// editorServesWorkspaceContent reports whether a path below an editor tab's prefix
// is one where the editor relays content it did not write:
//
//   - code-server's port proxy, proxy/<port>/… and absproxy/<port>/…, which serves
//     any loopback server (an agent's dev server) under the editor's origin;
//   - vscode-remote-resource, VS Code's route for worktree files (images, HTML,
//     SVG), which a repository or an agent controls.
//
// Both are matched as path SEGMENTS (rest is decoded and has no leading slash), the
// remote-resource one anywhere in the path because VS Code serves it under a
// versioned prefix. The workbench itself (its shell, static assets, webview hosts)
// matches neither and keeps the real origin it needs.
func editorServesWorkspaceContent(rest string) bool {
	segs := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	if len(segs) > 0 && (segs[0] == "proxy" || segs[0] == "absproxy") {
		return true
	}
	for _, s := range segs {
		if s == "vscode-remote-resource" {
			return true
		}
	}
	return false
}

// tabIsRelayedEditor reports whether (sessionID, tabID) is the VS Code tab of an
// off-box workspace, judged from the already-tracked session WITHOUT resolving the
// tab's target: resolving a relayed editor STARTS it on the workspace's host, and
// the mirror is about to refuse it, so it must not be started for nothing. The
// target's own RelayedEditor bit stays the authoritative check after resolution.
func (m *Manager) tabIsRelayedEditor(sessionID, tabID string) bool {
	if m == nil {
		return false
	}
	instance := m.trackedStreamSession(sessionID)
	if instance == nil {
		return false
	}
	kind, _, ok := instance.TabTargetByID(tabID)
	return ok && kind == session.TabKindVSCode && instance.Capabilities().Workspace == session.WorkspaceRemote
}

// relayedEditorNeedsOwnOrigin is the notice the mirror shows in place of a relayed
// editor (rule 2). It names both ways to give the editor an origin of its own.
const relayedEditorNeedsOwnOrigin = "This session's editor runs in its workspace, so af opens it only on an " +
	"origin of its own, never on the web UI's. To open it from this machine, set network.preview_listen_addr " +
	"to a fixed loopback port (for example `af config set network.preview_listen_addr 127.0.0.1:8444`). " +
	"To open it from another machine, set network.editor_origin_ports and expose those ports behind your " +
	"front door (see docs/web.md, per-session editor origins). Then reopen this tab."
