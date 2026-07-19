package session

import (
	"crypto/rand"
	"fmt"
	"sort"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// newTabID mints a stable, collision-free identity for a tab (#1738). Unlike a
// tab's ordinal position (which shifts on a reorder/close) or its name
// (which is reused on close+recreate — a fresh "shell" after the old one is
// gone), this id is minted once at creation, persisted, and never reused, so a
// stream or pane binding keyed on it can never misroute to a different tab after
// the tab list changes. It is a package var so tests can inject deterministic
// ids. crypto/rand is the entropy source; on the (near-impossible) read failure
// it falls back to a timestamp-derived value so tab creation never blocks on
// entropy — still unique per call in practice. 16 hex chars (64 bits) is ample
// for the handful of tabs an instance ever holds and keeps the id compact in a
// ?tab_id= query string.
var newTabID = func() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b[:])
}

// agentTabName is the canonical name of the default Agent tab — the handle a
// user types (`--name agent`), not the label the UI shows (which is "Agent";
// see TabLabel).
const agentTabName = "agent"

// shellTabName is the canonical name of the first Shell tab — the on-demand
// per-instance terminal session ('t' / `af sessions tab-create`). The handle a
// user types (`--name shell`), not the label the UI shows (which is "Terminal";
// see TabLabel).
const shellTabName = "shell"

// webTabName is the default canonical name of a web/iframe tab ('web', then
// 'web-2', … on collision). Created via `af sessions tab-create --kind web`. For
// web tabs the label equals the name (see TabLabel).
const webTabName = "web"

// vscodeTabName is the default canonical name of a VS Code tab ('vscode', then
// 'vscode-2', … on collision). Created via `af sessions tab-create --kind vscode`
// or the web UI's + New tab flow. For vscode tabs the label equals the name (see
// TabLabel).
const vscodeTabName = "vscode"

// tmuxTabSeparator joins an instance's agent tmux session name to a tab's name
// to derive that tab's own tmux session name (af_<repoHash>_<title>__<tab>).
// Deterministic so a tab's session is collision-free across instances and
// restorable by exact name across a restart. It is also the token boundary
// tabTmuxToken splits on to recover the name a live session was derived from,
// which a rename decouples from the tab's current name.
const tmuxTabSeparator = "__"

// shellTmuxSuffix extends an instance's agent tmux session name to derive its
// shell tab's session name (e.g. af_<repoHash>_<title>__shell). Deterministic
// so the shell session is collision-free across instances and restorable by
// exact name across a restart.
const shellTmuxSuffix = tmuxTabSeparator + shellTabName

// maxTabs is the soft cap on tabs per instance (#930 PR 4). It matches the 1-9
// number-key jump range: a session can hold the agent tab plus up to eight
// shell/process tabs, all reachable by a single number key.
const maxTabs = 9

// TabKind enumerates the kinds of process a Tab can host within an instance's
// worktree. PR 1 of the #930 ephemeral-tabs epic only materializes the Agent
// kind (the single per-instance agent session); Shell and Process are defined
// here so later PRs can add the human-spawned terminal tab and CLI-spawned
// process tabs without reshaping the type. See issue #930.
type TabKind int

const (
	// TabKindAgent is the agent session: the resolved agent program with
	// system-prompt injection, trust-prompt handling, and autoyes. Exactly one
	// per instance today, at Tabs[0].
	TabKindAgent TabKind = iota
	// TabKindShell is a plain $SHELL session in the worktree (the future
	// human-spawned terminal tab). Not created in PR 1.
	TabKindShell
	// TabKindProcess runs an arbitrary command in the worktree (the future
	// CLI-spawned tab). Not created in PR 1.
	TabKindProcess
	// TabKindWeb is a URL/iframe tab: it has NO tmux PTY and no process. It
	// carries a target URL (a loopback dev-server address the daemon
	// reverse-proxies, or an external absolute URL the web UI iframes directly)
	// so an agent can inject a live browser view into the user's screen. Rendered
	// as an iframe in the web UI and as a placeholder in the TUI (which cannot
	// render a browser). Created only through `af sessions tab-create --kind web`
	// / the CreateTab API — never a TUI hotkey.
	TabKindWeb
	// TabKindVSCode is a VS Code editor tab: a full code-server (or
	// openvscode-server) editor rooted at the session's WORKTREE, rendered as an
	// iframe in the web UI and as a placeholder in the TUI.
	//
	// Like TabKindWeb it has NO tmux PTY, and — deliberately — no URL either. The
	// editor process is DAEMON-managed and keyed by SESSION, not by tab: one
	// code-server per session, shared by every vscode tab in it, spawned lazily on
	// loopback with an EPHEMERAL port. Persisting a URL would therefore bake in a
	// port that is stale the moment the daemon restarts, so the target is resolved
	// dynamically at proxy time (Manager.WebTabTarget), which is also what makes
	// restore-then-respawn-lazily work with no stored state.
	//
	// Created through `af sessions tab-create --kind vscode` / the CreateTab API /
	// the web UI's + New tab flow — never a TUI hotkey. The target is ALWAYS the
	// session's worktree, so unlike a web tab it takes no --url/--port.
	TabKindVSCode
)

// HasTmux reports whether a tab of this kind owns a tmux PTY session — i.e.
// whether its persisted TmuxName is expected to be non-empty.
//
// Agent, shell, and process tabs run a real process behind a PTY. Web and vscode
// tabs are pure PROJECTIONS (an iframe target; a daemon-managed per-session
// code-server resolved at proxy time) and deliberately hold none. That property
// was stated only in the prose above, so every site that meets a tmux-less tab had
// to re-derive it from an empty TmuxName — and reading "" as "this tab's session
// is missing" rather than "this kind never had one" is what made an out-of-band
// web tab invisible in a running TUI (post-merge Codex finding on #1815). This is
// the single place that answers it.
//
// An unrecognized kind answers true: a tab claiming a process whose session can't
// be found is skipped by its caller, which is the conservative failure (a tab that
// doesn't show) rather than materializing a terminal tab with no PTY behind it.
func (k TabKind) HasTmux() bool {
	switch k {
	case TabKindWeb, TabKindVSCode:
		return false
	default:
		return true
	}
}

// Tab is one process running in an instance's worktree, backed by a single tmux
// session. It is an internal wrapper introduced in PR 1 of #930: an instance
// holds exactly one Agent tab that wraps today's single tmux session, and the
// instance's tmux-touching methods route through it. Tab lifecycle
// (create/close) and per-tab persistence land in later PRs; PR 1 keeps the
// on-disk format and all behavior unchanged.
type Tab struct {
	// ID is the tab's stable identity (#1738), minted at creation and persisted.
	// It is the collision-proof key streams and pane bindings address the tab by —
	// unlike the ordinal position (shifts on reorder/close) or the name
	// (reused on close+recreate). Empty only for a legacy persisted tab written
	// before #1738, which restoreLocalTabs backfills with a fresh id on load.
	ID string
	// Name is the tab's canonical handle: the stable, human-typable string a user
	// addresses it by (`agent`, `shell`, `btop`, or a name set at create/rename),
	// unique within the instance. It is what every tab verb resolves against
	// (`--name`, via TabMatches), alongside the stable ID above.
	//
	// It is NOT the display label. What the UI renders is TabLabel, a
	// presentation-only string that is never resolved against — the two
	// deliberately differ for agent/shell tabs (named `agent`/`shell`, shown as
	// "Agent"/"Terminal"). This field's doc once claimed to be "the display
	// label"; that confusion was #1986, and the split into Name (the one handle)
	// and TabLabel (the one display string) is its resolution.
	Name string
	// Kind selects the tab's process behavior.
	Kind TabKind
	// Command is the process to run; empty means the kind's default. Unused in
	// PR 1 — the agent program is still resolved by the local backend.
	Command string
	// URL is the target of a TabKindWeb tab: a normalized absolute URL, either a
	// loopback dev-server address (http://localhost:PORT) the daemon
	// reverse-proxies, or an external absolute URL the web UI iframes directly.
	// Empty for every other kind.
	URL string
	// Conversation is the provider-specific id that resumes this tab's agent
	// conversation exactly. Empty means recovery falls back to the provider's
	// existing latest-session behavior.
	Conversation AgentConversationData
	// tmux is the tab's tmux session. nil until the instance is started, and
	// always nil for remote/hook-backed instances, which drive their agent
	// session through hook commands rather than a local tmux session.
	tmux *tmux.TmuxSession
}

// newAgentTab returns the single Agent-kind tab that wraps an instance's tmux
// session.
func newAgentTab(ts *tmux.TmuxSession) *Tab {
	return &Tab{ID: newTabID(), Name: agentTabName, Kind: TabKindAgent, tmux: ts}
}

// newShellTab returns a Shell-kind tab named "shell" wrapping the given tmux
// session (a $SHELL process in the worktree). Shell tabs are created on demand
// ('t' / `af sessions tab-create`) — a fresh instance holds only its agent tab
// (#1100); the only automatic use is setupTabs replacing a persisted shell tab
// that restored dead (#991).
func newShellTab(ts *tmux.TmuxSession) *Tab {
	return &Tab{ID: newTabID(), Name: shellTabName, Kind: TabKindShell, tmux: ts}
}

// newRemoteAgentTab returns the Agent tab for a remote/hook-backed instance
// (#930 PR 6). Like every remote tab it carries no tmux session: the agent
// surface is driven by attach_cmd and the hook preview process, not a local
// tmux session. It lets remote instances be tab-driven through the same Tabs
// list as local ones.
func newRemoteAgentTab() *Tab {
	return &Tab{ID: newTabID(), Name: agentTabName, Kind: TabKindAgent}
}

// newWebTab returns a TabKindWeb tab pointing at url. It carries no tmux
// session (web tabs have no PTY): the target is rendered as an iframe in the web
// UI and as a placeholder in the TUI. The caller sets a unique name.
func newWebTab(url string) *Tab {
	return &Tab{ID: newTabID(), Name: webTabName, Kind: TabKindWeb, URL: url}
}

// newVSCodeTab returns a TabKindVSCode tab. It carries neither a tmux session nor
// a URL: the editor is a daemon-managed per-session code-server whose loopback
// address is resolved at proxy time (see TabKindVSCode). The caller sets a
// unique name.
func newVSCodeTab() *Tab {
	return &Tab{ID: newTabID(), Name: vscodeTabName, Kind: TabKindVSCode}
}

// tabKindNames maps the CreateTabRequest.Kind / `--kind` wire value to the
// TabKind it selects. It is the SINGLE source of truth for that vocabulary: the
// CLI (api/sessions_tabs.go) validates against it and the daemon
// (daemon/manager_tabs.go) dispatches on it, so the two can no longer drift into
// the state where a kind the CLI accepts is one the daemon rejects.
//
// An empty kind is deliberately absent: it is not a kind but the DEFAULT
// (shell-or-process, chosen by the request's Shell flag), so callers handle "" on
// its own before consulting this map.
var tabKindNames = map[string]TabKind{
	"web":    TabKindWeb,
	"vscode": TabKindVSCode,
}

// ParseTabKindName resolves a `--kind` / CreateTabRequest.Kind wire value to its
// TabKind. ok is false for any unknown value AND for the empty string, which is
// the caller's shell/process default rather than a kind.
func ParseTabKindName(name string) (kind TabKind, ok bool) {
	k, ok := tabKindNames[name]
	return k, ok
}

// TabKindNameList returns the sorted `--kind` values that select an explicit tab
// kind, for help text and "expected one of …" error messages, so those strings
// are generated from the vocabulary rather than hand-maintained beside it.
func TabKindNameList() []string {
	names := make([]string, 0, len(tabKindNames))
	for name := range tabKindNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// tabKindForData clamps a persisted TabKind to a known value, defaulting to
// TabKindShell for any unexpected value written by a newer binary so a forward-
// incompatible record degrades to a plain shell tab rather than an agent tab.
func tabKindForData(k TabKind) TabKind {
	switch k {
	case TabKindAgent, TabKindShell, TabKindProcess, TabKindWeb, TabKindVSCode:
		return k
	default:
		return TabKindShell
	}
}

// TabLabel returns the presentation-only string a user SEES for a tab — its
// display label. It is NEVER an identifier: no surface resolves a tab by it
// (TabMatches keys on Name alone), which is exactly what frees it to be a
// prettier, non-unique string than the canonical Name. Agent and shell tabs
// render fixed labels ("Agent", "Terminal") that are deliberately not their
// names (`agent`/`shell`); every other kind shows its Name.
//
// This is the #1986 split: Name is the one handle a user types, the label is the
// one string a user reads, and they are allowed to differ because the label
// carries no identity. It lives beside the Tab type — not in the TUI — so the
// definition of "what a user reads" sits next to Name, the definition of "what a
// user types": whenever the two differ, TabIdentifiers surfaces the label in a
// "no tab named …" error, so a user who read "Terminal" off the bar is told the
// real name `shell` rather than left to guess it. That discoverability is what
// replaces the label-as-alias #1937 shipped (#1984): the label never resolves,
// but it is never a dead end either.
func TabLabel(t *Tab) string {
	if t == nil {
		return ""
	}
	switch t.Kind {
	case TabKindAgent:
		return "Agent"
	case TabKindShell:
		return "Terminal"
	case TabKindWeb:
		if t.Name != "" {
			return t.Name
		}
		return "Web"
	case TabKindVSCode:
		if t.Name != "" {
			return t.Name
		}
		return "VS Code"
	default:
		if t.Name != "" {
			return t.Name
		}
		return "Tab"
	}
}

// TabMatches reports whether token identifies this tab. It keys on the canonical
// Name ONLY: the display label (TabLabel) is presentation and is never an
// identifier (#1986). A person who typed a label they read off the screen is not
// matched here — accepting it would make two strings address one tab, the
// ambiguity #1929/#1904 removed from the tab surface. The label is not a dead
// end either: TabIdentifiers names it in the resulting "no tab named …" error,
// so the user learns the real name to type (the discoverability half of #1984).
func TabMatches(t *Tab, token string) bool {
	if t == nil || token == "" {
		return false
	}
	return t.Name == token
}

// TabIdentifiers renders a tab as the strings that help a user address it in an
// error: its canonical Name, plus the label the UI shows when that differs.
// Because the label is presentation-only and never accepted (TabMatches keys on
// Name), naming it here is what lets a user who read "Terminal" off the bar find
// the `shell` they must type — the discoverability the #1986 split relies on in
// place of the label alias. Used to make "no tab named X" list the valid options
// instead of asserting an absence the user can see is false.
func TabIdentifiers(t *Tab) string {
	if t == nil {
		return ""
	}
	if label := TabLabel(t); label != t.Name {
		return fmt.Sprintf("%q (shown as %q)", t.Name, label)
	}
	return fmt.Sprintf("%q", t.Name)
}
