package ui

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
)

// WebUIHint is what a TabPane knows about where the daemon's web UI is served.
// The app resolves it off the render path — its daemon health poll — and hands
// it in through SetWebUIHint; a pane never probes for it in String.
//
// URL names the listener the daemon reports itself bound to ("http://…"), the
// only address the placeholders may print. Disabled marks a deliberately-off
// web server (network.listen_addr is ""), where no address exists to name. The
// zero value is "unknown": a remote target, a poll that has not landed or
// failed, and a daemon too old to report its listeners all land here, and the
// placeholders point at the docs rather than name an address they cannot prove.
type WebUIHint struct {
	URL      string
	Disabled bool
}

// SetWebUIHint installs the provider the web/vscode tab placeholders ask where
// the daemon's web UI is. It is called off the bubbletea event loop as well as
// on it, so the provider must be race-safe and must not call back into the
// pane — the app's is an atomic read of its latest health poll (#7811).
func (p *TabPane) SetWebUIHint(hint func() WebUIHint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.webUIHint = hint
}

// webUI resolves the pane's current web UI hint through the installed provider.
// Caller holds p.mu, matching the isNaming read in creatingFallbackLocked. An
// uninstalled provider yields the unknown hint — the docs pointer, never a
// guessed address.
func (p *TabPane) webUI() WebUIHint {
	if p.webUIHint == nil {
		return WebUIHint{}
	}
	return p.webUIHint()
}

// webUITarget is the shared "where the web UI is" clause of the tab
// placeholders below. It never invents an address: a resolved hint prints the
// bound listener verbatim, and every unresolved state points at where the
// address is configured — printing the built-in default would name a wrong
// address for anyone who changed network.listen_addr (#7811).
func webUITarget(h WebUIHint) string {
	switch {
	case h.URL != "":
		return "at " + h.URL
	case h.Disabled:
		return "— the web server is off (network.listen_addr is empty)"
	default:
		return "— see `af daemon --help` for its address"
	}
}

// webTabPlaceholder is the TUI content for a web/iframe tab, which the terminal
// cannot render: the target URL plus a pointer to where it can be viewed. Shared
// by updateShell and canEnterScrollModeLocked so the two never diverge.
func webTabPlaceholder(url string, h WebUIHint) string {
	return fmt.Sprintf("%s\n\nWeb tab — open the URL in a browser or view it in the web UI %s.", url, webUITarget(h))
}

// vscodeTabPlaceholder is the TUI content for a VS Code tab. Unlike a web tab
// there is no URL to show: the editor is a daemon-managed per-session
// code-server on a 0600 unix socket, reachable only through the daemon's proxy
// (#1873), so the only meaningful pointer is the web UI itself.
func vscodeTabPlaceholder(h WebUIHint) string {
	return fmt.Sprintf("Open this VS Code tab in the web UI %s.", webUITarget(h))
}

// docsTabPlaceholder is the TUI pane for a proposal Docs tab. The editor is web
// only, so the pane names the proposal and the way to read it from a terminal.
func docsTabPlaceholder(slug string) string {
	return fmt.Sprintf("The Proposal tab is web only. Open it in the web UI to edit proposal %s.\n\nIn a terminal, run: af proposal path %s", slug, slug)
}

// tabPlaceholder returns the TUI placeholder for a tab kind the terminal cannot
// render, and ok=false for kinds it can (agent/shell/process, which have a PTY).
// Shared by updateShell and canEnterScrollModeLocked so the two never diverge.
func tabPlaceholder(tab *session.Tab, h WebUIHint) (string, bool) {
	switch tab.Kind {
	case session.TabKindWeb:
		return webTabPlaceholder(tab.URL, h), true
	case session.TabKindVSCode:
		return vscodeTabPlaceholder(h), true
	case session.TabKindDocs:
		return docsTabPlaceholder(tab.Proposal), true
	default:
		return "", false
	}
}
