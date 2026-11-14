package session

import (
	"errors"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// recordedBoundary narrows an unverified committed re-run's cover to the
// boundary af durably recorded for the carried file (#7270). kept is the whole
// destination the re-run kept: the most it could attribute to the source,
// because nothing vouched for any of it.
//
// The watermark only ever grows, so covering kept when af already holds a
// record would turn the target's own appends past that record into source
// provenance for good. The recorded boundary is the widest span any record
// names (handoff.Snapshot.CarriedBoundary): the watermark, or else a
// quarantine marker for a carry whose watermark never landed. A zero
// watermark is a record — that carry copied nothing. Only a file af holds no
// record of at all keeps the whole-destination cover, as does one under its
// home's untrackable quarantine flag (#7277) or one whose records cannot be
// read: over-quarantine is the safe direction.
func (c *conversationCarry) recordedBoundary(kept int64) int64 {
	path := c.carriedTranscriptPath()
	if c.dstRoot.base == "" || path == "" {
		return kept
	}
	instances := filepath.Join(c.dstRoot.base, "instances")
	statePath := filepath.Join(instances, handoff.StateFileName)
	snap, stateErr, railsErr := handoff.LoadSnapshot(handoff.NewStore(statePath),
		handoff.NewRailsStore(filepath.Join(instances, handoff.RailsFileName), statePath))
	if err := errors.Join(stateErr, railsErr); err != nil {
		log.WarningLog.Printf("account swap re-run of %s conversation %s could not read the boundary af recorded for %s, so it counts the whole file as the previous account's: %v",
			c.agent, c.id, path, err)
		return kept
	}
	boundary, recorded := snap.CarriedBoundary(path)
	if !recorded {
		return kept
	}
	return min(boundary, kept)
}

// codexCarryReasonOf words carryFileAt's source refusals for a Codex rollout.
// carryFileAt does not know the agent, so it words them for a transcript.
var codexCarryReasonOf = map[string]string{
	carryTranscriptMissingReason: carryRolloutMissingReason,
	carryTranscriptEmptyReason:   carryRolloutEmptyReason,
}

// inAgentTerms names the file a refused copy is about the way the agent's
// plan-time refusals already do: a Codex rollout, a Claude transcript.
func (c *conversationCarry) inAgentTerms(err error) error {
	if c.agent != tmux.ProgramCodex {
		return err
	}
	var carryErr *carryError
	if !errors.As(err, &carryErr) {
		return err
	}
	if reason, ok := codexCarryReasonOf[carryErr.reason]; ok {
		return carryFailure(reason, carryErr.err)
	}
	return err
}
