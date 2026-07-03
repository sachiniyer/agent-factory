package session

import "github.com/sachiniyer/agent-factory/session/tmux"

// agentTabName is the display label of the default Agent tab.
const agentTabName = "agent"

// shellTabName is the display label of the first Shell tab — the on-demand
// per-instance terminal session ('t' / `af sessions tab-create`).
const shellTabName = "shell"

// shellTmuxSuffix extends an instance's agent tmux session name to derive its
// shell tab's session name (e.g. af_<repoHash>_<title>__shell). Deterministic
// so the shell session is collision-free across instances and restorable by
// exact name across a restart.
const shellTmuxSuffix = "__shell"

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
)

// Tab is one process running in an instance's worktree, backed by a single tmux
// session. It is an internal wrapper introduced in PR 1 of #930: an instance
// holds exactly one Agent tab that wraps today's single tmux session, and the
// instance's tmux-touching methods route through it. Tab lifecycle
// (create/close) and per-tab persistence land in later PRs; PR 1 keeps the
// on-disk format and all behavior unchanged.
type Tab struct {
	// Name is the display label (e.g. "agent").
	Name string
	// Kind selects the tab's process behavior.
	Kind TabKind
	// Command is the process to run; empty means the kind's default. Unused in
	// PR 1 — the agent program is still resolved by the local backend.
	Command string
	// tmux is the tab's tmux session. nil until the instance is started, and
	// always nil for remote/hook-backed instances, which drive their agent
	// session through hook commands rather than a local tmux session.
	tmux *tmux.TmuxSession
}

// newAgentTab returns the single Agent-kind tab that wraps an instance's tmux
// session.
func newAgentTab(ts *tmux.TmuxSession) *Tab {
	return &Tab{Name: agentTabName, Kind: TabKindAgent, tmux: ts}
}

// newShellTab returns a Shell-kind tab named "shell" wrapping the given tmux
// session (a $SHELL process in the worktree). Shell tabs are created on demand
// ('t' / `af sessions tab-create`) — a fresh instance holds only its agent tab
// (#1100); the only automatic use is setupTabs replacing a persisted shell tab
// that restored dead (#991).
func newShellTab(ts *tmux.TmuxSession) *Tab {
	return &Tab{Name: shellTabName, Kind: TabKindShell, tmux: ts}
}

// newRemoteAgentTab returns the Agent tab for a remote/hook-backed instance
// (#930 PR 6). Like every remote tab it carries no tmux session: the agent
// surface is driven by attach_cmd and the hook preview process, not a local
// tmux session. It lets remote instances be tab-driven through the same Tabs
// list as local ones.
func newRemoteAgentTab() *Tab {
	return &Tab{Name: agentTabName, Kind: TabKindAgent}
}

// newRemoteTerminalTab returns the Shell-kind terminal tab for a remote instance
// whose terminal_cmd hook is configured (#930 PR 6). It has no tmux session;
// attaching and previewing route through the terminal_cmd hook flow
// (HookBackend.AttachTerminal). Only created when HasTerminalCmd() is true, so a
// remote instance without terminal_cmd carries just the agent tab.
func newRemoteTerminalTab() *Tab {
	return &Tab{Name: shellTabName, Kind: TabKindShell}
}

// tabKindForData clamps a persisted TabKind to a known value, defaulting to
// TabKindShell for any unexpected value written by a newer binary so a forward-
// incompatible record degrades to a plain shell tab rather than an agent tab.
func tabKindForData(k TabKind) TabKind {
	switch k {
	case TabKindAgent, TabKindShell, TabKindProcess:
		return k
	default:
		return TabKindShell
	}
}
