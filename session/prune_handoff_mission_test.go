package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file pins the structural disqualifier PruneSkipReason must apply to an
// archived row carrying an undelivered handoff mission (#5136/#4429). Before the
// fix, the gate's only protection was the loader's op-axis fence reconstruction
// (instance_data.go's pendingHandoffMissionNeedsFence), which deliberately does
// not apply to ambiguous verdicts — and even a fenced verdict escapes the op
// axis once the row is read in its serialized form (ForStorage scrubs InFlightOp).
// The table row in prune_test.go covers the canonical disqualifier entry; these
// tests pin the two properties the table row alone cannot: the full verdict
// matrix in the serialized form, and the structural-precedes-age ordering the
// function's own comment promises.

// missionVerdictCases is the full verdict matrix a pending-handoff-mission
// archived row can land on. Each is a designed, archivable state — not a torn
// write — so every verdict must refuse the row, not just the two the daemon
// happens to fence on load.
func missionVerdictCases() []struct {
	name   string
	status PromptDeliveryStatus
	op     InFlightOp
} {
	return []struct {
		name   string
		status PromptDeliveryStatus
		op     InFlightOp
	}{
		// Ambiguous verdicts load without the OpReplacing fence, so pre-fix they
		// reached the gate with InFlightOp == OpNone and passed through eligible.
		{"could not confirm", PromptCouldNotConfirm, OpNone},
		{"sent unverified", PromptSentUnverified, OpNone},
		// The fenced verdicts reconstruct OpReplacing in memory, but a SERIALIZED
		// row scrubs the transient op — so PruneSkipReason (a pure function of
		// InstanceData, the shape #5189's apply will mirror) must refuse on the
		// mission itself, not on the op axis.
		{"not delivered serialized", PromptNotDelivered, OpNone},
		{"delivered serialized", PromptDelivered, OpNone},
		// A legacy record written before the verdict field existed carries no
		// status at all; the loader normalizes that ambiguity to could-not-confirm
		// and the gate must refuse it for the same reason.
		{"legacy no verdict", "", OpNone},
	}
}

// TestPruneSkipReason_PendingHandoffMissionRefusedAcrossVerdicts drives
// PruneSkipReason on a serialized-shape InstanceData (InFlightOp == OpNone) for
// every verdict the pending-handoff-mission field can carry. Every verdict must
// be refused by the obligation term, never fall through to eligibility — the op
// axis is reasserted at OpNone on each case so the protection exercised is the
// mission itself, not a reconstructed fence.
func TestPruneSkipReason_PendingHandoffMissionRefusedAcrossVerdicts(t *testing.T) {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	for _, tc := range missionVerdictCases() {
		t.Run(tc.name, func(t *testing.T) {
			data := pruneEligibleData(t)
			data.PendingHandoffMission = "continue the inherited work"
			data.HandoffDeliveryStatus = tc.status
			data.InFlightOp = tc.op

			reason := PruneSkipReason(data, cutoff)
			require.NotEmpty(t, reason,
				"a pending-handoff-mission archived row must not be a prune candidate (verdict %q, op %v)", tc.status, tc.op)
			assert.Contains(t, reason, "handoff mission undelivered",
				"the refusal must name the obligation, not a coincidental op-axis or age verdict")
			assert.NotContains(t, reason, "operation in flight",
				"the op axis is OpNone — the refusal must not be mislabeled as a busy op")
			assert.NotContains(t, reason, "archived too recently",
				"the obligation is structural and must precede the age test")
		})
	}
}

// TestPruneSkipReason_PendingHandoffMissionPrecedesAgeVerdict pins the
// function's own ordering promise: "structural disqualifiers come before the
// age test so a row that could never be reclaimed does not also report a
// confusing age verdict." A mission-owing archived row whose archive time is
// fresh must report the obligation, not "archived too recently" — the operator
// must not be told to wait out a cutoff the row can never become eligible under.
func TestPruneSkipReason_PendingHandoffMissionPrecedesAgeVerdict(t *testing.T) {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	data := pruneEligibleData(t)
	data.PendingHandoffMission = "continue the inherited work"
	data.HandoffDeliveryStatus = PromptCouldNotConfirm
	data.ArchivedAt = time.Now().Add(-24 * time.Hour) // fresh — would fail the age test alone

	reason := PruneSkipReason(data, cutoff)
	require.NotEmpty(t, reason)
	assert.Contains(t, reason, "handoff mission undelivered",
		"the mission disqualifier must win over the age test")
	assert.NotContains(t, reason, "archived too recently",
		"a structurally disqualified row must not also report a confusing age verdict")
}
