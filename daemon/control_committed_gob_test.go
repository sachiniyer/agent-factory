package daemon

import (
	"bytes"
	"encoding/gob"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/stretchr/testify/require"
)

// The control socket is net/rpc with gob, and gob ELIDES zero values. That is
// why MutationOutcome uses value types: a *bool would arrive nil at false and be
// indistinguishable from "never set", turning a committed outcome back into a
// clean failure — the exact bug the envelope exists to prevent (#1700, #3036).
//
// This asserts the property the design depends on, not the encoder: a committed
// outcome survives a real gob round trip through the concrete response types,
// and a clean one stays clean rather than arriving as an ambiguous zero.
func TestMutationOutcomeSurvivesGobRoundTrip(t *testing.T) {
	t.Run("committed outcome arrives intact", func(t *testing.T) {
		sent := KillSessionResponse{OK: true}
		sent.Code = apiproto.ErrorCodeMutationCommitted
		sent.Warning = "kill committed, but teardown failed"

		var got KillSessionResponse
		var buf bytes.Buffer
		require.NoError(t, gob.NewEncoder(&buf).Encode(sent))
		require.NoError(t, gob.NewDecoder(&buf).Decode(&got))

		committed, _ := got.CommittedOutcome()
		require.True(t, committed,
			"a committed kill that decodes as not-committed tells the caller to retry an applied change")
		require.Equal(t, "kill committed, but teardown failed", got.Warning)
	})

	// The other half, and the one a pointer field would break: a clean response
	// must decode as NOT committed. With a *bool both cases arrive nil, so this
	// pair is what distinguishes "false" from "absent".
	t.Run("clean outcome is not committed", func(t *testing.T) {
		var got KillSessionResponse
		var buf bytes.Buffer
		require.NoError(t, gob.NewEncoder(&buf).Encode(KillSessionResponse{OK: true}))
		require.NoError(t, gob.NewDecoder(&buf).Decode(&got))

		committed, _ := got.CommittedOutcome()
		require.False(t, committed, "a clean kill must never read as committed")
		require.Empty(t, got.Warning)
	})

	// Every mutating response embeds it; a new one that forgets loses the channel
	// silently, so the carrier check is asserted structurally rather than per type.
	t.Run("every mutating response carries the envelope", func(t *testing.T) {
		for name, resp := range map[string]any{
			"KillSession":     &KillSessionResponse{},
			"ArchiveSession":  &ArchiveSessionResponse{},
			"RestoreSession":  &RestoreSessionResponse{},
			"RestoreArchived": &RestoreArchivedResponse{},
			"AddTask":         &AddTaskResponse{},
			"UpdateTask":      &UpdateTaskResponse{},
			"RemoveTask":      &RemoveTaskResponse{},
		} {
			_, ok := resp.(interface{ CommittedOutcome() (bool, string) })
			require.True(t, ok, "%s response must embed MutationOutcome, or the client cannot read its outcome", name)
		}
	})
}

// The GENERAL case, and the property the deleted per-RPC classifier never had:
// a response type that no client, list, or switch has ever heard of still
// round-trips a committed outcome, purely by embedding MutationOutcome. If this
// ever needs a per-method branch to pass, the mechanism has regressed to what
// #3032 was closed for.
type unlistedMutationResponse struct {
	OK bool
	MutationOutcome
}

func TestUnlistedResponseTypeStillRoundTripsCommitted(t *testing.T) {
	sent := unlistedMutationResponse{OK: true}
	sent.Code = apiproto.ErrorCodeMutationCommitted
	sent.Warning = "committed, follow-up failed"

	var got unlistedMutationResponse
	var buf bytes.Buffer
	require.NoError(t, gob.NewEncoder(&buf).Encode(sent))
	require.NoError(t, gob.NewDecoder(&buf).Decode(&got))

	// Read exactly as the control client reads it: through the exported accessor
	// on the embedded struct, with no knowledge of this type.
	carrier, ok := any(&got).(interface{ CommittedOutcome() (bool, string) })
	require.True(t, ok, "embedding MutationOutcome must be all a response needs to opt in")
	committed, warning := carrier.CommittedOutcome()
	require.True(t, committed, "an unlisted response type must still report its committed outcome")
	require.Equal(t, "committed, follow-up failed", warning)
}
