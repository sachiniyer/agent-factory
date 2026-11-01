package daemon

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
)

// Workspace content never runs with the control origin's authority, nor with an
// editor's.
//
// The /v1/webtab/ mirror serves a session's dev server or editor on the SAME origin
// as the web UI, which holds the bearer token in localStorage and passes every
// same-origin check the daemon makes (the cross-site guard admits it as its own).
// So anything a workspace serves there — a dev server's page, a file from the
// worktree, an editor relayed from a sandbox — must not execute script as that
// origin. Rules 1 and 2 are judged on the mirror only (route.previewOrigin is
// false): the per-tab preview origin and the editor-origin ports are separate
// origins by construction. Rules 3 and 4 protect the EDITOR, whose origin is worth
// as much as a terminal, so they hold on every route that serves one.
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
//  3. Everything an editor serves, except its own workbench, is sandboxed on EVERY
//     route that serves an editor — the mirror, the preview listener's editor
//     origin and an editor-origin port alike. The rule is an ALLOW-list of the
//     workbench's paths (editorPathKeepsItsOrigin), not a list of the routes that
//     relay content: code-server's router matches case-insensitively, so a list
//     of bad routes (/proxy/) misses their other spellings (/PROXY/), and every
//     editor release can add one. What keeps a real origin is only what the
//     workbench needs one for, matched exactly, in the request's own encoding.
//     Everything else — code-server's port proxy, VS Code's worktree-file route,
//     anything unknown — answers as an opaque-origin document. On the mirror the
//     reason is the control origin; on an editor's own origin it is the editor
//     itself: a page at the editor's origin is same-origin with its WebSocket,
//     which is a terminal in that workspace.
//
//     What this does NOT cover: VS Code renders webviews (notebook outputs,
//     extension views) inside its webview host, a workbench page, in a frame it
//     gives allow-same-origin, and code-server serves that host from its own
//     static route. So a webview runs with the workbench's origin — the control
//     origin, for a local editor on the mirror. Closing that needs the local
//     editor on an origin of its own by default, which is a separate decision.
//
//  4. An editor's WebSocket is accepted only from the editor's own origin, on every
//     route that serves an editor (editorUpgradeRefusal). The editor's channel IS a
//     terminal in the workspace, and openvscode-server checks no Origin on it:
//     handleUpgrade reads none, and under --without-connection-token (which af
//     passes, since the socket's permissions authenticate the daemon) the token
//     check always passes. A browser does not apply CORS to a WebSocket handshake,
//     so without this a page on any other origin — a dev server on another port of
//     the same host, or a page rule 3 made opaque — could open that terminal
//     wherever it could reach the route. code-server has checked Origin on its
//     WebSockets since 4.10.1; this gives both editors the same rule, enforced by
//     af. "Own origin" is originNamesRequest's, the same comparison the cross-site
//     guard (crosssite.go) applies to the web UI's own streams, so an editor
//     connects behind exactly the front doors the web UI does. A handshake with no
//     Origin is not a browser's (the Fetch standard sends one on every WebSocket
//     handshake) and is admitted, as the cross-site guard and code-server both do.

//  5. No workspace response acts on a whole origin it shares (confineOriginHeaders).
//     Clear-Site-Data wipes the storage of the origin the RESPONSE came from,
//     whatever document asked for it, so a sandboxed page fetching a mirror URL
//     could clear the web UI's token, and one on an editor origin the editor's
//     state; it is stripped everywhere but a web tab's own preview origin, which
//     belongs to that app. Service-Worker-Allowed widens how far a service worker
//     registered from a script may reach, so on the mirror it is clamped to the
//     tab's own prefix: no tab's script can become a worker for the web UI at "/",
//     which would outlive any fix.

// mirrorSandboxCSP is the opaque-origin sandbox for mirrored workspace documents.
// Its flags must equal the web client's mirror iframe sandbox
// (web/src/tabaddr.ts webSandbox(false, false)), so a page behaves the same framed
// and top-level; TestMirrorSandboxMatchesTheClientsFrameSandbox and its TS twin pin
// both to this literal.
const mirrorSandboxCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals"

// workspaceSandboxPolicy is the CSP the proxy adds to a response, or "" for none:
// rule 1 for a web tab on the mirror (a web tab's own preview origin is separate
// from everything else by design and keeps a real origin), and rule 3 for every
// editor path outside the workbench, on any route.
func workspaceSandboxPolicy(route webTabRoute, kind session.TabKind) string {
	switch kind {
	case session.TabKindWeb:
		if !route.previewOrigin {
			return mirrorSandboxCSP
		}
	case session.TabKindVSCode:
		if !editorPathKeepsItsOrigin(route.escapedRest) {
			return mirrorSandboxCSP
		}
	}
	return ""
}

// editorPathKeepsItsOrigin reports whether a path below an editor tab's prefix is
// the editor's own workbench, the only part of an editor that needs a real origin
// (rule 3). escapedRest is the path in the request's OWN encoding, and segments are
// compared exactly, case included: the editor sees that same spelling, so a
// spelling only a case-insensitive or decoding router would route — "/STATIC/",
// "%73tatic" — is not the workbench here and is sandboxed. The workbench is:
//
//   - the shell, at the root (VS Code's "/"; code-server's too, and its "/vscode");
//   - VS Code's own files under static/: scripts, styles, workers, and the
//     webview host page;
//   - VS Code's sign-in callback page, callback;
//
// each optionally under VS Code's <quality>-<commit> product segment, which its
// server strips; and code-server's own assets under _static/, at its root. Both
// static trees are files from the editor's install, not from the workspace.
func editorPathKeepsItsOrigin(escapedRest string) bool {
	segs := strings.Split(strings.TrimPrefix(escapedRest, "/"), "/")
	if len(segs) >= 2 && segs[0] == "_static" {
		return true
	}
	if segs[0] == "vscode" {
		segs = segs[1:]
	}
	if len(segs) > 0 && editorProductSegment.MatchString(segs[0]) {
		segs = segs[1:]
		if len(segs) == 0 {
			return false // "/stable-…" with no slash is no route of VS Code's
		}
	}
	switch {
	case len(segs) == 0, len(segs) == 1 && segs[0] == "":
		return true
	case len(segs) >= 2 && segs[0] == "static":
		return true
	case len(segs) == 1 && segs[0] == "callback":
		return true
	}
	return false
}

// editorProductSegment is VS Code's server product path segment, <quality>-<commit>
// (getServerProductSegment), such as stable-<40 hex>; "oss-dev" for an unbuilt one.
var editorProductSegment = regexp.MustCompile(`^[a-z]+-(?:[0-9a-f]{7,64}|dev)$`)

// confineOriginHeaders is rule 5 on one upstream response. browserPath is the
// decoded path the browser requested, which a relative Service-Worker-Allowed is
// resolved against.
func confineOriginHeaders(h http.Header, route webTabRoute, kind session.TabKind, browserPath string) {
	if !(route.previewOrigin && kind == session.TabKindWeb) {
		h.Del("Clear-Site-Data")
	}
	if route.prefix != "" && len(h.Values("Service-Worker-Allowed")) > 0 {
		h.Set("Service-Worker-Allowed", clampWorkerScope(h.Get("Service-Worker-Allowed"), route.prefix, browserPath))
	}
}

// clampWorkerScope maps a Service-Worker-Allowed value onto the mirror and keeps it
// inside the tab's prefix. An absolute path is the app's own path, which the mirror
// serves under the prefix (like a redirect's Location); a relative one resolves
// against the script's URL as the browser would. Whatever lands outside the
// prefix, an absolute URL included, becomes the prefix itself.
func clampWorkerScope(value, prefix, browserPath string) string {
	floor := prefix + "/"
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return floor
	}
	scope := u.Path
	if strings.HasPrefix(scope, "/") {
		scope = prefix + scope
	} else {
		scope = path.Join(path.Dir(browserPath), scope)
	}
	scope = path.Clean(scope)
	if scope == prefix || !strings.HasPrefix(scope, floor) {
		return floor
	}
	if strings.HasSuffix(u.Path, "/") || u.Path == "" {
		scope += "/"
	}
	return scope
}

// trackedEditorTab reports whether (sessionID, tabID) is a VS Code tab, and whether
// its editor is RELAYED from an off-box workspace, judged from the already-tracked
// session WITHOUT resolving the tab's target: resolving an editor STARTS it (on the
// workspace's host, for a relayed one), and rules 2 and 4 refuse before that, so an
// editor is never started only to be turned away. The session is addressed the way
// WebTabTarget addresses it — a stable id, else a title unique across projects —
// so the claim holds for either spelling of the route. The resolved target's Kind
// and RelayedEditor stay the authoritative checks after resolution, for a session
// that is not tracked yet.
func (m *Manager) trackedEditorTab(sessionID, tabID string) (editor, relayed bool) {
	if m == nil {
		return false, false
	}
	instance := m.trackedWebTabSession(sessionID)
	if instance == nil {
		return false, false
	}
	kind, _, ok := instance.TabTargetByID(tabID)
	if !ok || kind != session.TabKindVSCode {
		return false, false
	}
	return true, instance.Capabilities().Workspace == session.WorkspaceRemote
}

// trackedWebTabSession is the tracked half of WebTabTarget's session resolution
// (resolveStreamSession, unscoped): the stable id first, then a title that exactly
// one project holds. It never refreshes from disk; an ambiguous or unknown name is
// nil here and is left to the resolution, which reports it.
func (m *Manager) trackedWebTabSession(idOrTitle string) *session.Instance {
	if instance := m.trackedStreamSession(idOrTitle); instance != nil || idOrTitle == "" {
		return instance
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var match *session.Instance
	var repos []string
	for key, instance := range m.instances {
		if instance == nil || instance.Title != idOrTitle {
			continue
		}
		rid, _ := splitDaemonInstanceKey(key)
		repos = append(repos, rid)
		match = instance
	}
	if len(session.DedupeSorted(repos)) != 1 {
		return nil
	}
	return match
}

// editorUpgradeRefusal is rule 4 for one request on an editor tab's route: a
// protocol upgrade whose Origin is not the origin the request was addressed to is
// refused. Every upgrade counts, not only "websocket": httputil.ReverseProxy relays
// any Upgrade the upstream accepts, and a browser cannot set Upgrade or Connection
// itself, so judging every request that names one costs no legitimate request.
// allowedHosts is network.allowed_hosts, for originNamesRequest's X-Forwarded-Host
// admission behind a same-host proxy.
func editorUpgradeRefusal(r *http.Request, allowedHosts []string) *crossSiteDenial {
	if !requestsUpgrade(r) {
		return nil
	}
	origin := r.Header.Get("Origin")
	if origin == "" || originNamesRequest(origin, r, allowedHosts) {
		return nil
	}
	return &crossSiteDenial{
		status: http.StatusForbidden,
		err: fmt.Errorf("refused an editor connection from Origin %q: an editor accepts connections only "+
			"from its own page. Behind a reverse proxy that rewrites Host, forward the browser's with "+
			"X-Forwarded-Host (and X-Forwarded-Proto) or keep Host", echoHost(origin)),
		logKey: "editor-origin:" + echoHost(origin),
	}
}

// requestsUpgrade reports whether r asks for a protocol switch: an Upgrade header,
// or an "upgrade" token in Connection. Broader than ReverseProxy's own test (both),
// so nothing it would relay as an upgrade escapes the check.
func requestsUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// webTabAllowedHosts is network.allowed_hosts for this request: from the posture
// the control listener's Host guard already built for it (one config read per
// request), else from the live config: the preview listener and the editor-origin
// ports put no posture in the context.
func (cs *controlServer) webTabAllowedHosts(r *http.Request) []string {
	if p, ok := requestPostureFrom(r.Context()); ok {
		return p.allowedHosts
	}
	return editorPortAllowedHosts(cs)
}

// relayedEditorNeedsOwnOrigin is the notice the mirror shows in place of a relayed
// editor (rule 2). It names both ways to give the editor an origin of its own.
const relayedEditorNeedsOwnOrigin = "This session's editor runs in its workspace, so af opens it only on an " +
	"origin of its own, never on the web UI's. To open it from this machine, set network.preview_listen_addr " +
	"to a fixed loopback port (for example `af config set network.preview_listen_addr 127.0.0.1:8444`). " +
	"To open it from another machine, set network.editor_origin_ports and expose those ports behind your " +
	"front door (see docs/web.md, per-session editor origins). Then reopen this tab."
