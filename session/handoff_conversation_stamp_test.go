package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #6768: the pending account-swap record holds the conversation outcome only
// until its delivery obligation retires — ClearPendingAccountSwap and
// ConfirmPendingAccountSwapDelivery both delete it. The answer the
// caller reads afterwards lives on the handoff ledger entry, so retiring the
// record must stamp it there first.
func TestPendingSwapRetirementStampsTheLedgerConversation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending *AccountSwapData
		want    HandoffConversation
	}{
		{
			name: "carried",
			pending: &AccountSwapData{
				Manual: true, From: "personal", To: "work",
				CarriedConversationID: "019f386f-7206-7fc2-803b-f7045e07a242",
			},
			want: HandoffConversation{Carried: true},
		},
		{
			name: "carry fell back to a fresh start",
			pending: &AccountSwapData{
				Manual: true, From: "personal", To: "work",
				CarryFallback: "the outgoing account's transcript could not be copied",
			},
			want: HandoffConversation{CarryFailure: "the outgoing account's transcript could not be copied"},
		},
		{
			name: "cross-agent records no carry",
			pending: &AccountSwapData{
				Manual: true, From: "personal", To: "work",
			},
			want: HandoffConversation{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &Instance{Title: "t"}
			inst.Tabs = []*Tab{{Handoffs: []AgentHandoff{{ToAccount: "work"}}}}
			inst.pendingAccountSwap = tc.pending

			require.True(t, inst.ClearPendingAccountSwap("personal", "work"))
			require.Equal(t, tc.want, inst.Tabs[0].Handoffs[0].Conversation)
			_, _, pending := inst.PendingAccountSwap()
			require.False(t, pending, "the pending record is gone; the ledger holds the answer")
		})
	}
}

// An automatic swap records no ledger entry, and a pending record whose target
// is not the trailing entry's must not stamp the wrong one.
func TestPendingSwapRetirementStampsOnlyItsOwnEntry(t *testing.T) {
	inst := &Instance{Title: "t"}
	inst.Tabs = []*Tab{{Handoffs: []AgentHandoff{{ToAccount: "work"}}}}
	inst.pendingAccountSwap = &AccountSwapData{
		From: "personal", To: "work", // not Manual: automatic swaps never append
		CarriedConversationID: "019f386f-7206-7fc2-803b-f7045e07a242",
	}
	require.True(t, inst.ClearPendingAccountSwap("personal", "work"))
	require.Equal(t, HandoffConversation{}, inst.Tabs[0].Handoffs[0].Conversation)

	inst.Tabs = []*Tab{{Handoffs: []AgentHandoff{{ToAccount: "other"}}}}
	inst.pendingAccountSwap = &AccountSwapData{
		Manual: true, From: "personal", To: "work",
		CarryFallback: "x",
	}
	require.True(t, inst.ClearPendingAccountSwap("personal", "work"))
	require.Equal(t, HandoffConversation{}, inst.Tabs[0].Handoffs[0].Conversation,
		"the entry's ToAccount is not this swap's target")
}

// The operator-attestation path retires the same record and owes the same
// stamp.
func TestConfirmPendingAccountSwapDeliveryStampsTheLedger(t *testing.T) {
	inst := &Instance{Title: "t"}
	inst.Tabs = []*Tab{{Handoffs: []AgentHandoff{{ToAccount: "work"}}}}
	inst.pendingAccountSwap = &AccountSwapData{
		Manual: true, From: "personal", To: "work",
		ReplacementPanesStarted: true,
		MissionDeliveryStatus:   PromptDelivered,
		CarryFallback:           "its transcript is missing",
	}
	require.NoError(t, inst.ConfirmPendingAccountSwapDelivery("personal", "work"))
	require.Equal(t,
		HandoffConversation{CarryFailure: "its transcript is missing"},
		inst.Tabs[0].Handoffs[0].Conversation)
	_, _, pending := inst.PendingAccountSwap()
	require.False(t, pending)
}
