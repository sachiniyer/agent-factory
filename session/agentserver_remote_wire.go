package session

import (
	"encoding/json"

	"github.com/sachiniyer/agent-factory/terminal"
)

// --- wire structs: the /v1/agent/* request/response shapes (mirror PR1) ---
//
// These match daemon/agentserver_headless.go's agent* types field-for-field (the
// server side). They are duplicated as unexported session types rather than
// imported because daemon imports session (a cycle the other way); the shared
// contract is the JSON tags, which the round-trip test pins.

type agentLifecycleReq struct {
	FirstTimeSetup bool `json:"first_time_setup"`
	// Conversation is the daemon's recorded conversation for a launch to resume
	// (#5571 W7.2). Absent for a first launch and for a pre-field server.
	Conversation *AgentConversationData `json:"conversation,omitempty"`
	// RestartAdoption marks the launch that adopts a RESTARTED agent-server's
	// surviving workspace (W1.4). Only that launch is refused a lazy respawn onto
	// a guessed conversation; every other restore (an archive restore, a Lost
	// recovery) keeps resuming the way it always has.
	RestartAdoption bool `json:"restart_adoption,omitempty"`
	// SiblingTabs is how many non-agent tabs the daemon's row records, sent with a
	// restart adoption's Provision so the host can refuse to rebuild under them.
	SiblingTabs int `json:"sibling_tabs,omitempty"`
	// Tabs is the roster of off-box shell/process tabs a restarted server should
	// rebind to their surviving tmux sessions (feature tab-restore, #6162). Sent
	// only to a server that advertises the feature; the commands in it are never run.
	Tabs []TabData `json:"tabs,omitempty"`
}

// agentLaunchResp carries back the conversation the server launched or resumed.
type agentLaunchResp struct {
	Conversation *AgentConversationData `json:"conversation,omitempty"`
	// TabsRestored and TabsGone answer a restart adoption's Tabs: the ids rebound
	// to a live session, and the ids whose session tmux confirmed absent.
	TabsRestored []string `json:"tabs_restored,omitempty"`
	TabsGone     []string `json:"tabs_gone,omitempty"`
	// SetupHookFailed relays an in-sandbox setup-payload overlay failure the
	// remote worktree create recorded (#6667).
	SetupHookFailed string `json:"setup_hook_failed,omitempty"`
}

type agentSnapshotResp struct {
	Updated     bool              `json:"updated"`
	Baseline    bool              `json:"baseline"`
	HasPrompt   bool              `json:"has_prompt"`
	Content     string            `json:"content"`
	ModelChange *AgentModelChange `json:"model_change,omitempty"`
	// Captured is a pointer so "server predates the field" stays
	// distinguishable from "server reports its capture failed" (#5221
	// review): nil resolves to the legacy contract — an all-false frame is
	// unconfirmed, positive evidence marks the capture real.
	Captured *bool `json:"captured,omitempty"`
	// Reflow marks a changed capture attributed to a daemon-driven viewer
	// resize (#7434). A plain bool suffices: a server that predates the field
	// reports nothing, and absent resolves to the same false the legacy
	// contract already meant — a resize is never attributed, so churn keeps
	// answering attention exactly as it did before the field existed.
	Reflow bool `json:"reflow,omitempty"`
	// DialogHold is the sandbox gate's settled hold (#7635). Optional and
	// additive: a server that predates it omits it, which reads as no hold.
	//
	// Kept raw and decoded on its own (decodeWireDialogHold): a notice that
	// does not decode — a wrong type today, a changed shape from a newer
	// server tomorrow — must read as no hold, never fail the frame around it.
	DialogHold json.RawMessage `json:"dialog_hold,omitempty"`
	// Conversation is the conversation identity the server has for the agent, once
	// it has one (#5571 W7.2).
	Conversation *AgentConversationData `json:"conversation,omitempty"`
}

type agentPreviewReq struct {
	// TabID addresses by stable id and wins over Tab, mirroring the daemon's
	// own PreviewRequest precedence — a stale id is a tab_gone 404, never an
	// ordinal fallback (#5571 W4.3).
	TabID string `json:"tab_id,omitempty"`
	Tab   int    `json:"tab"`
	Full  bool   `json:"full"`
	Grid  bool   `json:"grid,omitempty"`
}

type agentPreviewResp struct {
	Content         string         `json:"content"`
	Modes           terminal.Modes `json:"terminal_modes,omitempty"`
	HasModes        bool           `json:"has_terminal_modes,omitempty"`
	LinesAbove      int            `json:"lines_above,omitempty"`
	LinesAboveKnown bool           `json:"lines_above_known,omitempty"`
	HasGrid         bool           `json:"has_grid,omitempty"`
	Grid            string         `json:"grid,omitempty"`
	HasCursor       bool           `json:"has_cursor,omitempty"`
	CursorRow       int            `json:"cursor_row,omitempty"`
	CursorCol       int            `json:"cursor_col,omitempty"`
	HasVisibility   bool           `json:"has_visibility,omitempty"`
	CursorVisible   bool           `json:"cursor_visible,omitempty"`
	HasSize         bool           `json:"has_size,omitempty"`
	Rows            uint16         `json:"rows,omitempty"`
	Cols            uint16         `json:"cols,omitempty"`
}

type agentAliveResp struct {
	Alive bool `json:"alive"`
}

type agentSendPromptReq struct {
	Prompt string `json:"prompt"`
}

type agentSendPromptResp struct {
	OK     bool                 `json:"ok"`
	Status PromptDeliveryStatus `json:"status"`
}

// agentBoundDraftStillStagedResp mirrors
// daemon/agentserver_headless.go's agentBoundDraftStillStagedResponse — the
// closed enum alone crosses the wire; the daemon phrases it (#7546).
type agentBoundDraftStillStagedResp struct {
	Read StagedDraftRead `json:"read"`
}

type agentArchiveResp struct {
	Branch string `json:"branch"`
}

// --- wire structs: the W4.3 stable-id tab surface (#5571) ---
//
// These mirror daemon/agentserver_headless.go's agentTab*/agentHealth* types
// field-for-field, the same duplication rule as the rest of this file: the JSON
// tags are the contract, and the round-trip tests pin them.

// agentTabCreateReq asks the sandbox to spawn a tab under the daemon's stable
// id. Kind is the tabKindVocabulary wire name — only "shell" and "process" are
// creatable here. ID is verbatim AND the idempotency key: a retry repeating
// id+kind (and command, for a process tab) answers the tab the first create
// made; a same-id request of a different shape refuses. Empty ID mints.
type agentTabCreateReq struct {
	ID      string `json:"id,omitempty"`
	Kind    string `json:"kind"`
	Command string `json:"command,omitempty"`
	Name    string `json:"name,omitempty"`
}

// agentTabCreateResp carries the created tab as the sandbox's roster row — the
// same TabData shape /v1/agent/tabs reports, so the create answer can never
// drift from the roster.
type agentTabCreateResp struct {
	Tab TabData `json:"tab"`
}

// agentTabCloseReq names the tab to close by stable id only.
type agentTabCloseReq struct {
	ID string `json:"id"`
	// Gen makes the close conditional on the tab's incarnation, checked
	// atomically on the sandbox (#5996). Empty closes whatever holds the id.
	Gen string `json:"gen,omitempty"`
}

// agentTabsResp is the sandbox's roster: every tab as TabData, exit evidence
// included.
type agentTabsResp struct {
	Tabs []TabData `json:"tabs"`
}

// agentHealthResp is /v1/health's payload: liveness plus the version/features
// advertisement. Features is nil against a server that predates the field —
// the caller degrades on a missing TOKEN, and a missing field is just zero
// tokens.
type agentHealthResp struct {
	OK       bool     `json:"ok"`
	Version  string   `json:"version"`
	Features []string `json:"features"`
}
