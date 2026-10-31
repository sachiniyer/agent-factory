package session

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
)

// Guidance generated from capabilities (#5571 W3.3).
//
// The af reference an agent is injected with must teach only verbs that work for
// THAT session. The three things that vary are whether a callback exists (without
// one no af verb can reach the daemon — W3.1), which tab kinds the session's
// projection admits, and whether the session can archive itself. They are read
// from the same sources the rails and `sessions get` project from —
// callbackUnavailable and tabKindAllowances(Capabilities) — rather than a second
// list that could drift, and guidance_test.go pins the rendered text to that
// projection so a capability flipped later in W3–W5 updates the guidance with it.

// guidanceCaps is the capability projection the reference is rendered from.
// It is comparable so it can key a generated file's name.
type guidanceCaps struct {
	// Callback: an af credential reached the session. False renders the
	// callbackless variant, which teaches no af verb at all.
	Callback bool
	// SelfArchive: `af sessions archive --self` works — the session can archive
	// and the callback it needs to say so exists.
	SelfArchive bool
	// Shell, Web and VSCode are the tab kinds the session's projection admits.
	// A process tab is a shell tab's sibling (TabNeedsLocalProcess) and follows
	// Shell; the projection never lists it as its own kind.
	Shell, Web, VSCode bool
	// WebHTTPSOnly: the admitted web tabs are external HTTPS targets only — a
	// loopback target (--port) is refused, as it is on a remote workspace.
	WebHTTPSOnly bool
	// HostStores: the session's af calls reach the daemon host's proposal and
	// backlog stores. A local worktree's agent calls af over the host's own
	// socket; an agent in a remote workspace calls it with the scoped sandbox
	// credential, and no proposal or backlog route admits that credential
	// (daemon/httproutes.go: none is sandboxAllowed). False drops the proposal
	// and backlog verbs, and the proposal skill, from the guidance.
	HostStores bool
}

// fullGuidanceCaps is what a local session with a callback is shown — the
// reference af has always injected, byte for byte.
var fullGuidanceCaps = guidanceCaps{Callback: true, SelfArchive: true, Shell: true, Web: true, VSCode: true, HostStores: true}

// noCallbackGuidanceCaps is the W3.1 callbackless variant.
var noCallbackGuidanceCaps = guidanceCaps{}

// guidanceCapsFor projects capabilities onto what the guidance may mention.
func guidanceCapsFor(callbackUnavailable bool, c Capabilities) guidanceCaps {
	if callbackUnavailable {
		return noCallbackGuidanceCaps
	}
	g := guidanceCaps{Callback: true, SelfArchive: c.Archive, HostStores: c.Workspace == WorkspaceLocalWorktree}
	for _, a := range tabKindAllowances(c) {
		switch a.Kind {
		case "shell":
			g.Shell = a.Allowed
		case "web":
			g.Web = a.Allowed
			g.WebHTTPSOnly = a.Allowed && c.RefuseTabKind(TabKindWeb, "http://localhost:3000") != nil
		case "vscode":
			g.VSCode = a.Allowed
		}
	}
	return g
}

// guidanceCaps is the projection for this instance. An in-sandbox instance is
// built local (tmux + worktree) but the daemon holds its session as a remote
// workspace, and the daemon is who admits tabs — so it renders from the remote
// runtime's capabilities, not the local ones it happens to run on.
func (i *Instance) guidanceCaps() guidanceCaps {
	callbackUnavailable := i.CallbackUnavailable() != ""
	i.mu.RLock()
	hosted, advertised := i.hostedRemotely, i.advertisedFeatures
	i.mu.RUnlock()
	if hosted {
		// The remote backend's declaration is only the floor; the daemon lifts it
		// for a sandbox advertising the tab API (capabilitiesLocked, W4.5), so the
		// guidance lifts it by the same advertisement (#5878).
		caps := (&remoteAgentBackend{}).Capabilities()
		if slices.Contains(advertised, tabAPIFeature) {
			caps.TabManagement = true
		}
		return guidanceCapsFor(callbackUnavailable, caps)
	}
	return guidanceCapsFor(callbackUnavailable, i.Capabilities())
}

// key names the variant in a generated file or directory. The two original
// variants keep their historical names so existing installs do not churn.
func (g guidanceCaps) key() string {
	switch g {
	case fullGuidanceCaps:
		return ""
	case noCallbackGuidanceCaps:
		return "callbackless"
	}
	sum := sha256.Sum256([]byte(g.String()))
	return hex.EncodeToString(sum[:4])
}

func (g guidanceCaps) String() string {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	return "cb" + b(g.Callback) + "sa" + b(g.SelfArchive) + "sh" + b(g.Shell) + "w" + b(g.Web) + "ws" + b(g.WebHTTPSOnly) + "vs" + b(g.VSCode) + "hs" + b(g.HostStores)
}

// afSelfArchiveLine and afUsageOutroInsideNoSelfArchive are the two places the
// inside reference names `archive --self`.
const (
	afSelfArchiveLine               = "\n  af sessions archive --self                            Archive your OWN session (resolved via whoami); no title needed"
	afUsageOutroInsideNoSelfArchive = `Finishing up: when the user confirms your work is complete and asks you to wrap up, say so — this session cannot archive itself, so the user archives it. Do not run "af sessions archive" on your own session.`
)

// Reference renders the inside reference for these capabilities. A session that
// can do everything gets afUsageReference unchanged.
func (g guidanceCaps) Reference() string {
	if !g.Callback {
		return afUsageReferenceCallbackless
	}
	out := afUsageReference
	if !g.SelfArchive {
		out = strings.Replace(out, afSelfArchiveLine, "", 1)
		out = strings.Replace(out, afUsageOutroInside, afUsageOutroInsideNoSelfArchive, 1)
	}
	if !g.HostStores {
		// Each section is its own paragraph, so it goes with the blank line
		// that separates it from the next one.
		for _, section := range []string{afBacklogSection, afUsageProposalsInside, afUsageBacklogInside} {
			out = strings.Replace(out, section+"\n\n", "", 1)
		}
	}
	if g.Shell && g.Web && g.VSCode {
		return out
	}
	return strings.Replace(out, afTabsSectionFull, g.tabsSection(), 1)
}

// afTabsSectionFull is the tab section exactly as afUsageBody renders it.
const afTabsSectionFull = afTabsLead + afTabsRestrictions + afTabsTail + "\n" +
	afTabLineShell + "\n" + afTabLineProcess + "\n" + afTabLineWeb + "\n" +
	afTabLineVSCode + "\n" + afTabLineDelete

// tabsSection renders only the tab verbs the projection admits. The
// "requires a local session" prose is dropped with the lines it explained —
// what is admitted is now what is listed. With no admitted kind the whole
// section goes: a delete verb for tabs the agent cannot create is noise.
func (g guidanceCaps) tabsSection() string {
	var lines []string
	if g.Shell {
		lines = append(lines, afTabLineShell, afTabLineProcess)
	}
	if g.Web {
		lines = append(lines, g.webLine())
	}
	if g.VSCode {
		lines = append(lines, afTabLineVSCode)
	}
	if len(lines) == 0 {
		return "Tabs: this session cannot add tabs; ask the user if you need a terminal, web, or editor view."
	}
	lines = append(lines, afTabLineDelete)
	lead := afTabsLead
	if g.Web && g.WebHTTPSOnly {
		// Keep the restriction the full text carries: a remote workspace admits only
		// external HTTPS web tabs, and the daemon refuses a loopback target.
		lead += afTabsWebHTTPSOnly
	}
	return lead + afTabsTail + "\n" + strings.Join(lines, "\n")
}

// afTabsWebHTTPSOnly replaces the full text's restriction prose for a session
// whose only admitted web targets are external HTTPS URLs.
const afTabsWebHTTPSOnly = "Web tabs on this session accept only external HTTPS URLs; a loopback target (localhost or a local port) is refused. "

// afTabLineWebHTTPSOnly is afTabLineWeb without the --port alternative, which a
// remote workspace refuses.
const afTabLineWebHTTPSOnly = "  af sessions tab-create <title> --kind web --url <https-url>     Web tab — a live browser view (no process) shown to the user in the web UI; this is how you show an external HTTPS site"

func (g guidanceCaps) webLine() string {
	if g.WebHTTPSOnly {
		return afTabLineWebHTTPSOnly
	}
	return afTabLineWeb
}
