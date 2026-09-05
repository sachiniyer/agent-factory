package bugreport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// The usage-limit account swap (#3127) added five account LABELS to
// InstanceData, and a label is the string a user picked — "work", "personal",
// or the name of an employer or client. They take the same trade Account
// already takes (#3051/#3588): the marker keeps the triage fact that an
// identity was in play and drops what it was called.
//
// TestRedactInstanceDataCoversEveryStringField is the guard that CATCHES a new
// field, and it caught these — but it is a meta-test over the field set, so it
// stays satisfied by a classification. This asserts the redaction actually runs.
func TestRedactInstanceDataRedactsAccountSwapLabels(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	d := session.InstanceData{
		ID:           "abc123",
		Program:      "claude",
		Status:       session.Status(1),
		Account:      "acme-prod",
		LimitAccount: "acme-prod",
		PendingAccountSwap: &session.AccountSwapData{
			From:           "acme-prod",
			To:             "acme-staging",
			ConversationID: "8f466d20-784b-4b02-a916-c80a0f6983e3",
		},
		AccountLimitObservations: []session.AccountLimitObservationData{
			{Agent: "claude", Account: "acme-prod", ResetAt: reset},
		},
	}

	redactOneInstanceData(&d)

	for name, got := range map[string]string{
		"Account":                             d.Account,
		"LimitAccount":                        d.LimitAccount,
		"PendingAccountSwap.From":             d.PendingAccountSwap.From,
		"PendingAccountSwap.To":               d.PendingAccountSwap.To,
		"AccountLimitObservations[0].Account": d.AccountLimitObservations[0].Account,
	} {
		if got != redactedMarker {
			t.Errorf("%s not redacted: %q", name, got)
		}
	}
	// Cleared, not marked — like AgentConversation.ID. A resumable handle's
	// VALUE is the sensitive part, and its presence is not worth reporting.
	if d.PendingAccountSwap.ConversationID != "" {
		t.Errorf("replacement conversation id not cleared: %q", d.PendingAccountSwap.ConversationID)
	}
	// The structural fields triage actually reads survive, including the agent
	// enum beside the redacted label — without it the observation list is two
	// opaque markers rather than "two claude accounts".
	if d.ID != "abc123" || d.Program != "claude" || d.Status != session.Status(1) {
		t.Errorf("structural fields mutated: %+v", d)
	}
	if d.AccountLimitObservations[0].Agent != "claude" {
		t.Errorf("agent enum redacted; it is bounded and load-bearing for triage: %q",
			d.AccountLimitObservations[0].Agent)
	}
	if !d.AccountLimitObservations[0].ResetAt.Equal(reset) {
		t.Errorf("reset time mutated: %v", d.AccountLimitObservations[0].ResetAt)
	}
}

// The #2419 fallback guard, for the same fields. A legacy or corrupt record that
// fails the typed decode takes the generic path, where the field-level policy
// above cannot apply — so a record af could not parse must not be LESS private
// than one it could. Before the keys were listed, none of these was a secret
// pattern, a path, or a known title, so the closing text scrub would not have
// caught any of them.
func TestRedactInstancesFallbackRedactsAccountSwapLabels(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{
		"id":"leg-1","status":"legacy-string-status","program":"claude",
		"limit_account":"acme-prod",
		"pending_account_swap":{"from":"acme-prod","to":"acme-staging","conversation_id":"8f466d20-784b"},
		"account_limit_observations":[{"agent":"claude","account":"acme-prod"}]
	}]`)
	out := string(r.redactInstancesJSON(raw))
	for _, leaked := range []string{"acme-prod", "acme-staging", "8f466d20-784b"} {
		if strings.Contains(out, leaked) {
			t.Errorf("fallback path leaked %q:\n%s", leaked, out)
		}
	}
	// The KEYS survive with a marker, which is what keeps the fallback useful:
	// "a swap was pending" and "an identity was walled" are still readable.
	for _, kept := range []string{"limit_account", "pending_account_swap", "account_limit_observations"} {
		if !strings.Contains(out, kept) {
			t.Errorf("fallback path dropped the whole %q key, losing the triage fact:\n%s", kept, out)
		}
	}
	// The bounded agent enum is not a label and is not redacted on this path either.
	if !strings.Contains(out, `"claude"`) {
		t.Errorf("fallback path redacted the agent enum:\n%s", out)
	}
}
