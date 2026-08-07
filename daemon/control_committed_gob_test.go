package daemon

import (
	"bytes"
	"encoding/gob"
	"testing"

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
		sent.Committed = true
		sent.Warning = "kill committed, but teardown failed"

		var got KillSessionResponse
		var buf bytes.Buffer
		require.NoError(t, gob.NewEncoder(&buf).Encode(sent))
		require.NoError(t, gob.NewDecoder(&buf).Decode(&got))

		require.True(t, got.Committed,
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

		require.False(t, got.Committed, "a clean kill must never read as committed")
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
			_, ok := resp.(interface{ committedOutcome() (bool, string) })
			require.True(t, ok, "%s response must embed MutationOutcome, or the client cannot read its outcome", name)
		}
	})
}
