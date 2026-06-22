package session

import "github.com/sachiniyer/agent-factory/session/tmux"

// agentTabName is the display label of the default Agent tab.
const agentTabName = "agent"

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
