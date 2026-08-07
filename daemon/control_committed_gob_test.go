package daemon

import (
	"bytes"
	"encoding/gob"
	"testing"

	"encoding/json"
	"errors"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/stretchr/testify/require"
	"net/rpc"
	"strings"
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

// TestLegacyDaemonCommittedWarningSurvivesSkew pins the old-daemon -> new-client
// direction: a pre-#3036 daemon sends `warning` with no `code`. Measured: gob
// matches the promoted field name and json flattens via the tag, so the value
// lands in the envelope on both transports and must read as COMMITTED. Without
// the code-less branch this is an ordinary success and the hook failure is lost.
func TestLegacyDaemonCommittedWarningSurvivesSkew(t *testing.T) {
	type legacyArchiveResponse struct {
		ArchivedPath string
		Warning      string
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(legacyArchiveResponse{
		ArchivedPath: "/archived/here",
		Warning:      "on-archive hook failed",
	}); err != nil {
		t.Fatalf("encode legacy response: %v", err)
	}
	var got ArchiveSessionResponse
	if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatalf("decode into current response: %v", err)
	}
	if got.ArchivedPath != "/archived/here" {
		t.Errorf("payload lost across skew: got %q", got.ArchivedPath)
	}
	committed, warning := got.CommittedOutcome()
	if !committed {
		t.Fatal("a code-less warning from an older daemon read as a CLEAN success; " +
			"the committed hook failure would be silently dropped")
	}
	if warning != "on-archive hook failed" {
		t.Errorf("warning = %q, want the legacy text", warning)
	}
}

// TestLegacyRPCErrorStillClassifiesCommitted pins the task path, which answers
// with an ERROR -- net/rpc sends no response body then, so the envelope never
// runs. An older daemon's committed marker survives only as a flattened
// rpc.ServerError string (#2512). Reading it as an ordinary failure invites a
// retry that duplicates an already-durable task.
func TestLegacyRPCErrorStillClassifiesCommitted(t *testing.T) {
	for _, prefix := range committedPrefixes {
		err := rpc.ServerError(prefix + " reload refused")
		classified := committedFromLegacyRPCError(err)
		if classified == nil {
			t.Fatalf("committed marker %q read as an ordinary failure", prefix)
		}
		if !isMutationCommitted(classified) {
			t.Errorf("classified error for %q is not marked committed", prefix)
		}
	}
	// A genuine clean failure must NOT be laundered into "committed".
	if got := committedFromLegacyRPCError(rpc.ServerError("task add failed: bad cron")); got != nil {
		t.Errorf("clean failure misclassified as committed: %v", got)
	}
	if got := committedFromLegacyRPCError(nil); got != nil {
		t.Errorf("nil error classified as committed: %v", got)
	}
}

// TestDeleteProjectCarriesCommittedOutcome pins the response that did NOT opt in
// and silently regressed to a clean success (#3041 review). Its json key is
// unchanged, so this is wire-compatible, not a new contract.
func TestDeleteProjectCarriesCommittedOutcome(t *testing.T) {
	resp := DeleteProjectResponse{OK: true, ArchivedCount: 2}
	resp.record(&mutationCommittedError{err: errors.New("on-archive hook failed")})
	committed, warning := resp.CommittedOutcome()
	if !committed {
		t.Fatal("DeleteProject committed outcome does not round-trip")
	}
	if warning == "" {
		t.Error("warning text lost")
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"warning":`) {
		t.Errorf("legacy `warning` json key disappeared, breaking old clients: %s", encoded)
	}
}
