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
		ID:             "abc123",
		Program:        "claude",
		RuntimeProgram: "/home/siyer/.local/bin/claude --dangerously-skip-permissions",
		Status:         session.Status(1),
		Account:        "acme-prod",
		LimitAgent:     "codex",
		LimitAccount:   "acme-prod",
		PendingAccountSwap: &session.AccountSwapData{
			From:                  "acme-prod",
			To:                    "acme-staging",
			AccountAgent:          "codex",
			ConversationID:        "8f466d20-784b-4b02-a916-c80a0f6983e3",
			CarriedConversationID: "5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d",
			CarrySourceAccount:    "acme-legacy",
			CarryFallback:         `the previous account "acme-legacy" is no longer registered for claude`,
			MissionDeliveryStatus: session.PromptCouldNotConfirm,
		},
		AccountLimitObservations: []session.AccountLimitObservationData{
			{Agent: "claude", Account: "acme-prod", ResetAt: reset},
		},
	}

	redactOneInstanceData(&d)

	for name, got := range map[string]string{
		"Account":                               d.Account,
		"LimitAccount":                          d.LimitAccount,
		"PendingAccountSwap.From":               d.PendingAccountSwap.From,
		"PendingAccountSwap.To":                 d.PendingAccountSwap.To,
		"PendingAccountSwap.CarrySourceAccount": d.PendingAccountSwap.CarrySourceAccount,
		"PendingAccountSwap.CarryFallback":      d.PendingAccountSwap.CarryFallback,
		"AccountLimitObservations[0].Account":   d.AccountLimitObservations[0].Account,
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
	if d.PendingAccountSwap.CarriedConversationID != "" {
		t.Errorf("carried conversation id not cleared: %q", d.PendingAccountSwap.CarriedConversationID)
	}
	// The structural fields triage actually reads survive, including the agent
	// enum beside the redacted label — without it the observation list is two
	// opaque markers rather than "two claude accounts".
	if d.ID != "abc123" || d.Program != "claude" || d.Status != session.Status(1) {
		t.Errorf("structural fields mutated: %+v", d)
	}
	if d.RuntimeProgram != "claude" {
		t.Errorf("runtime program did not redact to its bounded agent label: %q", d.RuntimeProgram)
	}
	if d.AccountLimitObservations[0].Agent != "claude" {
		t.Errorf("agent enum redacted; it is bounded and load-bearing for triage: %q",
			d.AccountLimitObservations[0].Agent)
	}
	if d.LimitAgent != "codex" {
		t.Errorf("limit agent enum redacted; it is bounded and identifies the quota provider: %q",
			d.LimitAgent)
	}
	if d.PendingAccountSwap.AccountAgent != "codex" {
		t.Errorf("pending swap namespace enum redacted; it is bounded and says which registry the redacted To lives in: %q",
			d.PendingAccountSwap.AccountAgent)
	}
	if d.PendingAccountSwap.MissionDeliveryStatus != session.PromptCouldNotConfirm {
		t.Errorf("mission delivery enum redacted; it is bounded and explains the retry fence: %q",
			d.PendingAccountSwap.MissionDeliveryStatus)
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
		"runtime_program":"/home/siyer/.local/bin/claude --dangerously-skip-permissions",
		"limit_agent":"codex","limit_account":"acme-prod",
		"account_agent":"acme-internal-agent",
		"pending_account_swap":{"from":"acme-prod","to":"acme-staging","conversation_id":"8f466d20-784b"},
		"account_limit_observations":[{"agent":"claude","account":"acme-prod"}]
	}]`)
	out := string(r.redactInstancesJSON(raw))
	for _, leaked := range []string{
		"acme-prod", "acme-staging", "8f466d20-784b", "acme-internal-agent",
		"/home/siyer/.local/bin/claude", "--dangerously-skip-permissions",
	} {
		if strings.Contains(out, leaked) {
			t.Errorf("fallback path leaked %q:\n%s", leaked, out)
		}
	}
	// The KEYS survive with a marker, which is what keeps the fallback useful:
	// "a swap was pending" and "an identity was walled" are still readable.
	for _, kept := range []string{
		"runtime_program", "limit_account", "pending_account_swap", "account_limit_observations",
	} {
		if !strings.Contains(out, kept) {
			t.Errorf("fallback path dropped the whole %q key, losing the triage fact:\n%s", kept, out)
		}
	}
	// The bounded agent enum is not a label and is not redacted on this path either.
	if !strings.Contains(out, `"claude"`) {
		t.Errorf("fallback path redacted the agent enum:\n%s", out)
	}
	if !strings.Contains(out, `"limit_agent": "codex"`) {
		t.Errorf("fallback path redacted the bounded limit agent enum:\n%s", out)
	}
}

// PendingAccountSwap.AccountAgent (#4430) is published only as the bounded
// agent enum af writes there. A value that is not one did not come from af —
// a hand-edited or foreign record — and is marked like the labels beside it.
func TestRedactInstanceDataMarksNonEnumAccountSwapNamespace(t *testing.T) {
	d := session.InstanceData{PendingAccountSwap: &session.AccountSwapData{
		To: "work", AccountAgent: "acme-internal-agent",
	}}
	redactOneInstanceData(&d)
	if d.PendingAccountSwap.AccountAgent != redactedMarker {
		t.Errorf("non-enum account namespace published verbatim: %q", d.PendingAccountSwap.AccountAgent)
	}
}
