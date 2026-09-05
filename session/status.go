package session

// Status is the legacy single-axis session status, kept because it SERIALIZES
// as an int (#1195): the two-axis liveness/in-flight-op model that replaced it is
// derived from these values by the GetStatus/SetStatus shim in liveness.go, and
// old records stay readable only while the enum is appended to and never
// renumbered. Split out of instance.go, unchanged, so that file has room to
// describe the Instance (#1145).

type Status int

const (
	// Running is the status when the instance is running and claude is working.
	Running Status = iota
	// Ready is if the claude instance is ready to be interacted with (waiting for user input).
	Ready
	// Loading is if the instance is loading (if we are setting it up).
	Loading
	// Deleting is if the instance is being torn down asynchronously after the
	// user confirmed a kill. Like Loading it is transient in-memory state: it
	// is never persisted (SaveInstances skips Loading/Deleting) and the row is
	// removed or reverted when the background teardown finishes (#844).
	Deleting
	// Dead is when the underlying tmux/remote session has vanished out from
	// under us (e.g. the tmux server was killed externally). The row is a
	// corpse: the user can no longer attach to it (handleEnter surfaces an
	// error instead of silently swallowing Enter) but can still kill it. A
	// dead session's HasUpdated latches (false,false) — the same value a
	// healthy idle session returns — so without an explicit liveness probe the
	// metadata tick would repaint it Ready (green dot) forever, making a corpse
	// masquerade as healthy (#935). Unlike Loading/Deleting this is NOT
	// in-flight TUI state: it is persisted and background syncs may still reap
	// or replace the row, so it is deliberately excluded from isTransientStatus.
	//
	// As of #1108 Dead is write-never: observed disappearance is recorded as
	// Lost instead, and FromInstanceData rewrites persisted Dead to Lost on
	// load (rollforward — the only writers of persisted Dead were
	// observed-death paths; user kills delete the record). The value stays in
	// the enum because Status serializes as an int: appending, never
	// renumbering, is what keeps old records readable.
	Dead
	// Lost is when the underlying tmux/remote session vanished out from under
	// a live session with no user kill on record — the tmux server was killed,
	// an outage/OOM starved it (#1104), or the box rebooted while the daemon
	// had already observed the death. Unlike a user-killed session (whose
	// record is deleted, with a UserKilled tombstone covering the teardown
	// crash window), a Lost session is wanted: it is recovery-eligible and the
	// daemon restores it best-effort (#1108). Persisted, like Dead; excluded
	// from isTransientStatus for the same reason.
	Lost
	// Archived is the deliberate counterpart of Lost (#1028): the user ran
	// `af sessions archive`, so the daemon tore down every tmux session (agent
	// + shell/process tabs; web tabs have no tmux and survive with their URLs,
	// #1809) and MOVED the worktree out to the global archive
	// dir (<AGENT_FACTORY_HOME>/archived/<repoID>/<title>/). Where Lost is a
	// wanted, self-healing state until its bounded restore attempts give up (tmux
	// vanished under a live record), Archived is a wanted,
	// QUIESCENT state: it is never probed, never marked Lost, and never
	// auto-restored — only an explicit `af sessions restore` moves the worktree
	// back and re-spawns the agent. It therefore loads INERT (FromInstanceData
	// skips Start: no tmux binding, started=false), which is what keeps the
	// status poll (skips !Started), the Lost-restore loop (gates on ==Lost),
	// and the root ensure loop from touching it. Persisted, like Dead/Lost;
	// appended, never renumbered (Status serializes as an int), so old records
	// stay readable — the same rollforward discipline #658/#1108 rely on.
	Archived
)
