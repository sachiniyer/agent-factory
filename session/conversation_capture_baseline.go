package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// SessionCodexCaptureBaseline builds the capture snapshot a deferred arming
// needs when the launch-time one is gone — after a daemon restart, or for a
// session that predates capture. It binds the store and working directory to
// the command that actually launched the live runtime — durable evidence a
// program_overrides edit cannot move (#6769 review) — falling back to a fresh
// resolution only for rows that predate runtime evidence.
func (i *Instance) SessionCodexCaptureBaseline() ConversationCaptureSnapshot {
	snap, err := i.sessionCodexCaptureBaseline(log.WarningLog.Printf)
	if err != nil {
		log.WarningLog.Printf("session %q: %v", i.Title, err)
	}
	return snap
}

// RetakeSessionCodexCaptureBaseline is SessionCodexCaptureBaseline for a
// caller that takes the baseline again on a cadence — the capture loop after a
// failed before-image — and so must not log the same failure on every call:
// it returns why there is no baseline, and the caller's per-error dedup warns
// once (#7131). A project config the repository answered as invalid still
// degrades to the global one, as the arm's resolve does, but the retake does
// not report that degrade again on every rearm (#7140): reporting it is the
// launch-class resolve's job, which the arm and every launch already do.
func (i *Instance) RetakeSessionCodexCaptureBaseline() (ConversationCaptureSnapshot, error) {
	return i.sessionCodexCaptureBaseline(nil)
}

// sessionCodexCaptureBaseline is the shared body; warn reports a degraded
// project config, and nil drops the report.
func (i *Instance) sessionCodexCaptureBaseline(warn func(string, ...any)) (ConversationCaptureSnapshot, error) {
	program := i.RuntimeProgram()
	if strings.TrimSpace(program) == "" {
		// An unreadable project config is "cannot baseline", the empty
		// snapshot every other unresolvable store answers — never a store
		// bound to the global config's command (#7077).
		resolution, err := resolveLaunchProgramForInstanceReporting(i, warn)
		if err != nil {
			return ConversationCaptureSnapshot{}, fmt.Errorf("no conversation capture baseline: %w", err)
		}
		program = resolution.command
	}
	return i.sessionCodexCaptureBaselineFor(program), nil
}

// sessionCodexCaptureBaselineFor builds the snapshot against an explicit
// launch command. Only a local workspace can answer it: a remote pane writes
// its provider store inside its own sandbox, the daemon's account registry is
// a DIFFERENT store, and a remote row's empty worktree would leave rollout
// correlation unscoped — so a stray local rollout could be recorded as the
// remote session's identity. Remote identities arrive through the
// agent-server reporting path in conversation_remote.go (#6769 review).
func (i *Instance) sessionCodexCaptureBaselineFor(program string) ConversationCaptureSnapshot {
	home, workingDir, ok := i.sessionCodexCaptureStoreFor(program)
	if !ok {
		return ConversationCaptureSnapshot{}
	}
	return BeginConversationCaptureAtCodexHomeAndWorkingDir(home, workingDir)
}
