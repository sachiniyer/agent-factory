package session

import "time"

// TabData is the serializable form of a session.Tab. The full list is persisted
// (and restored by exact TmuxName) so every tab — agent and shell alike —
// reconnects to its tmux session across an af/daemon restart (#930). The field
// is omitempty + additive, mirroring the BranchCreatedByUs back-compat
// precedent: instances.json written before #930 PR 2 simply has no Tabs, and
// FromInstanceData synthesizes [agent, shell] from the legacy TmuxName/Program.
// TabKindAllowance is the projected verdict for one creatable tab kind.
type TabKindAllowance struct {
	// Kind is the `--kind` spelling, the same vocabulary the CLI validates against
	// (session.TabKindNameList), so a client switches on the name the user types.
	Kind string `json:"kind"`
	// Allowed is RefuseTabKind returning nil for this kind on this session.
	Allowed bool `json:"allowed"`
	// Reason is the daemon's refusal text, empty when allowed. Rendered verbatim by
	// clients; it names the requirement that is actually unmet (#3053).
	Reason string `json:"reason,omitempty"`
}

type TabData struct {
	// ID is the tab's stable identity (#1738), minted at creation and never
	// reused. It is the collision-proof key the PTY stream (?tab_id=) and the web
	// DnD/pane bindings address the tab by, so a reorder/close can't misroute.
	// omitempty + additive, mirroring the InstanceData.ID / BranchCreatedByUs
	// rollforward precedent: a record written before #1738 has no id, and
	// restoreLocalTabs backfills a fresh one on load.
	ID string `json:"id,omitempty"`
	// Gen identifies this INCARNATION of the tab: minted when the process is
	// spawned, so a close-and-recreate under the same id (allowed) is a different
	// tab to anything that fences on it (#5571, #5955, #5960). Sent by the
	// agent-server on create and in its roster; the daemon records it and applies
	// exit evidence only to the incarnation that produced it. omitempty: rows from
	// before the field carry none and are never fenced in.
	Gen  string `json:"gen,omitempty"`
	Name string `json:"name"`
	// The wire names it as `kind_name`, the word `tab_kinds[].kind` uses too.
	Kind     TabKind `json:"kind"`
	Command  string  `json:"command,omitempty"`
	TmuxName string  `json:"tmux_name,omitempty"`
	// SandboxTmux is the tmux session a SANDBOX spawned for an off-box shell or
	// process tab, as it reported at create. Distinct from TmuxName, which binds a
	// session on the daemon's own host: this one names nothing the daemon can reach.
	// It is what a restarted agent-server is handed to rebind the survivor (W1.4,
	// #6162), so it survives a daemon restart.
	SandboxTmux string `json:"sandbox_tmux,omitempty"`
	// RestoreState is an off-box tab's standing after an agent-server restart
	// (see TabRestoreState): empty is restored, "unknown" and "gone" are debts the
	// daemon must remember across its own restart (#6235). Additive.
	RestoreState TabRestoreState `json:"restore_state,omitempty"`
	// URL is the target of a TabKindWeb tab (the iframe/proxy address); empty for
	// every other kind. Surfaced in the snapshot so the web UI can iframe it and
	// so `af sessions get` shows the target.
	URL string `json:"url,omitempty"`
	// Proposal is the slug a docs tab edits; empty for every other kind.
	Proposal string `json:"proposal,omitempty"`
	// PushMode is a docs tab's push mode, "auto" or "manual"; empty means auto.
	PushMode string `json:"push_mode,omitempty"`
	// WebProxied is derived at JSON encoding with IsLoopbackWebTarget, the
	// daemon proxy's predicate. A pointer preserves explicit false for web tabs;
	// non-web tabs omit it. Stored/input values never override the derivation.
	WebProxied *bool `json:"web_proxied,omitempty"`
	// Conversation is the provider-specific conversation id for this tab, when
	// the underlying agent exposes a durable resume id. Omitted for legacy rows
	// and providers where af can only resume "latest".
	Conversation *AgentConversationData `json:"conversation,omitempty"`
	// Handoffs is the tab's append-only agent-swap ledger (#2013), oldest first.
	// omitempty + additive on the same rollforward precedent as ID and
	// Conversation: a record written before #2013 has none, which is
	// indistinguishable from a session that was never handed off — and those two
	// deserve the same treatment, so nothing has to be backfilled.
	Handoffs []AgentHandoff `json:"handoffs,omitempty"`
	// Exit records how a process tab's command ended (#4479): the pane was seen
	// dead, not merely missing, or af stopped it and says why. nil for tabs af
	// never saw finish — the load path maps that to "still in flight", never to
	// "re-run me".
	Exit *TabExitData `json:"exit,omitempty"`
	// AccountScope is the account af launched this tab's pane under (#4506
	// review). A sibling whose recorded scope is not the session's account is
	// stopped once at load, because its pane may run on another identity. Empty
	// for a pane launched on the ambient identity, and for rows written before
	// this field existed.
	AccountScope string `json:"account_scope,omitempty"`
}

// TabExitData is the wire form of Tab.Exit: only the fields a reader needs to
// render or reason about a finished command.
type TabExitData struct {
	Status      int  `json:"status,omitempty"`
	StatusKnown bool `json:"status_known,omitempty"`
	// At is omitted, not zero-valued, when tmux reported no death time:
	// omitempty never omits a struct, and "0001-01-01T00:00:00Z" would present
	// an invented completion time to every reader (#4506 review).
	At time.Time `json:"at,omitzero"`
	// StoppedBy is set when af stopped the command rather than it exiting:
	// "account-scope" or "account-swap" (#4506 review).
	StoppedBy string `json:"stopped_by,omitempty"`
}

// TabCleanupData is one durable cleanup handle for a closed tab whose tmux
// teardown was never confirmed. It deliberately carries only what a retry needs
// — no Kind, Command, or URL — so it cannot be mistaken for, or restored as, a
// TabData: a tombstone that could round-trip into a tab would resurrect exactly
// the closed tab #2669 exists to keep buried.
type TabCleanupData struct {
	// TabID is the closed tab's stable id (#1738). Ids are never reused, so it
	// names the exact close this handle belongs to and keeps the retry's logs and
	// deduplication honest across restarts.
	TabID string `json:"tab_id,omitempty"`
	// TmuxName is the cleanup handle proper: the EXACT tmux session name the retry
	// must kill, and the token a later spawn must not re-derive. An entry with no
	// name would be untargetable, so CloseTab never records one.
	TmuxName string `json:"tmux_name"`
}

// RemoteTabCleanupData names an off-box tab whose close the sandbox has not
// confirmed. The stable id is the whole handle — the sandbox addresses its tabs
// by it — and Gen pins the incarnation, so a retry can never close a later tab
// that reused the id.
type RemoteTabCleanupData struct {
	TabID string `json:"tab_id"`
	Gen   string `json:"gen,omitempty"`
}
