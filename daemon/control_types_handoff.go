package daemon

import "time"

// HandoffStatusRequest asks for the auto-handoff rails: the configured mode,
// the durable halt/backoff/move records, and whether this daemon's engine is
// actually observing. It takes no arguments — the rails are this host's af
// home, exactly what `af accounts handoff status` reads through the daemon.
type HandoffStatusRequest struct{}

// HandoffBackoffEntry is one refusal backoff: the stable session identity
// the engine leaves alone until Until.
type HandoffBackoffEntry struct {
	SessionKey string    `json:"session_key"`
	Title      string    `json:"title,omitempty"`
	Until      time.Time `json:"until"`
}

// HandoffMoveEntry is one recorded automatic move — what the switch-loop
// rail counts per SessionKey and the anti-pile-on credits count per
// Agent + Account (#6776 review: "work" on codex is a different seat than
// "work" on claude, the same namespacing occupancy counts).
type HandoffMoveEntry struct {
	SessionKey string    `json:"session_key"`
	Title      string    `json:"title,omitempty"`
	Agent      string    `json:"agent,omitempty"`
	Account    string    `json:"account"`
	At         time.Time `json:"at"`
}

// HandoffStatusResponse is the rails state, server-side rendered into the
// vocabulary the status surface prints so the CLI, a remote client, and a
// web caller cannot disagree about what the file says.
type HandoffStatusResponse struct {
	// Mode is the resolved global [account_handoff] mode — "off" when the
	// table is absent (absent ≡ off ≡ today). Empty when the config could
	// not be read; ModeUnknown is then set so "off" is never guessed.
	Mode        string `json:"mode"`
	ModeUnknown bool   `json:"mode_unknown,omitempty"`
	// Engaged reports whether the daemon's engine is observing sessions
	// right now (any resolved config opted in). Records can exist with the
	// engine off — a state file outlives the mode that wrote it.
	Engaged bool `json:"engaged"`
	// Halted/HaltReason/HaltAt carry the durable halt record (R16: a halted
	// mover is never silent). Only engine-originated moves are stopped;
	// ordinary wait-for-reset resume is unaffected.
	Halted     bool       `json:"halted,omitempty"`
	HaltReason string     `json:"halt_reason,omitempty"`
	HaltAt     *time.Time `json:"halt_at,omitempty"`
	// ReportsHeldSince is the oldest standing report_to delivery hold.
	ReportsHeldSince *time.Time `json:"reports_held_since,omitempty"`
	// Backoffs are the live refusal backoffs, sorted by until then key.
	Backoffs []HandoffBackoffEntry `json:"backoffs,omitempty"`
	// RecentMoves are the recorded automatic moves, sorted by at then key.
	// The configured MaxMoves/MaxMovesWindow/RefusedBackoff/OnFailure ride
	// along so the reader can interpret the records against the same rules
	// the engine applies.
	RecentMoves    []HandoffMoveEntry `json:"recent_moves,omitempty"`
	MaxMoves       int                `json:"max_moves"`
	MaxMovesWindow string             `json:"max_moves_window,omitempty"`
	RefusedBackoff string             `json:"refused_backoff,omitempty"`
	OnFailure      string             `json:"on_failure,omitempty"`
	Warnings       []string           `json:"warnings,omitempty"`
}

// HandoffResetRequest clears one named rail record. Record names what to
// clear — "halt", "backoff", or "moves"; SessionKey scopes "backoff"/"moves"
// to one stable session identity (empty = every session's entries). The
// grammar is deliberately narrow: a reset clears ONLY what the operator
// names, so clearing a halt can never silently take the loop counters or a
// held report with it.
type HandoffResetRequest struct {
	Record     string `json:"record"`
	SessionKey string `json:"session_key,omitempty"`
}

// HandoffResetResponse reports what the reset actually did. Changed is false
// when the named record already stood empty — an idempotent reset is a
// truthful answer, not an error.
type HandoffResetResponse struct {
	Changed bool   `json:"changed"`
	Summary string `json:"summary"`
}
