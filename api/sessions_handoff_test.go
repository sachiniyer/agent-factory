package api

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

type committedHandoffError struct{}

func (committedHandoffError) Error() string           { return "handoff delivered, but settlement is pending" }
func (committedHandoffError) MutationCommitted() bool { return true }

func TestHandoffOutputOmitsAmbientAccounts(t *testing.T) {
	for _, tc := range []struct{ name, fromAccount, toAccount string }{
		{name: "agent-only"},
		{name: "ambient-to-pinned", toAccount: "personal"},
		{name: "pinned-to-pinned", fromAccount: "work", toAccount: "personal"},
		// #4428: a scoped session handed to a target with no account support
		// drops the scope — the drop is reported on from_account alone.
		{name: "scoped-dropped", fromAccount: "work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			oldCall := handoffSessionViaDaemon
			oldTo, oldBrief, oldAccount := sessionsHandoffTo, sessionsHandoffBrief, sessionsHandoffAccount
			oldRepo, oldEnvelope := repoFlag, envelopeOutput
			t.Cleanup(func() {
				handoffSessionViaDaemon = oldCall
				sessionsHandoffTo, sessionsHandoffBrief, sessionsHandoffAccount = oldTo, oldBrief, oldAccount
				repoFlag, envelopeOutput = oldRepo, oldEnvelope
			})
			sessionsHandoffTo, sessionsHandoffBrief, sessionsHandoffAccount = "claude", "", tc.toAccount
			repoFlag, envelopeOutput = "", false
			handoffSessionViaDaemon = func(req daemon.HandoffSessionRequest) (daemon.HandoffSessionResponse, error) {
				require.Equal(t, "worker", req.Title)
				require.Equal(t, tc.toAccount, req.Account)
				return daemon.HandoffSessionResponse{OK: true, From: "codex", To: "claude", HeadSHA: "branch-tip", FromAccount: tc.fromAccount, ToAccount: tc.toAccount}, nil
			}
			out := captureStdout(t, func() { require.NoError(t, sessionsHandoffCmd.RunE(sessionsHandoffCmd, []string{"worker"})) })
			var payload map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &payload))
			expected := map[string]any{"ok": true, "title": "worker", "from": "codex", "to": "claude", "head_sha": "branch-tip",
				// #6768: the response always answers the carry question; the
				// stub's zero value is the uncarried answer with no reason.
				"conversation": map[string]any{"carried": false}}
			if tc.fromAccount != "" {
				expected["from_account"] = tc.fromAccount
			}
			if tc.toAccount != "" {
				expected["to_account"] = tc.toAccount
			}
			require.Equal(t, expected, payload)
		})
	}
}

// #6768: the handoff output must carry the daemon's conversation verdict
// through verbatim — a carry that landed, and a fresh start's stated reason —
// so the caller never has to infer it from the other fields.
func TestHandoffOutputReportsTheConversationOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		conv session.HandoffConversation
		want map[string]any
	}{
		{
			name: "carried",
			conv: session.HandoffConversation{Carried: true},
			want: map[string]any{"carried": true},
		},
		{
			name: "carry fell back",
			conv: session.HandoffConversation{CarryFailure: "the outgoing account's transcript could not be copied"},
			want: map[string]any{"carried": false, "carry_failure": "the outgoing account's transcript could not be copied"},
		},
		{
			name: "different program",
			conv: session.HandoffConversation{CarryFailure: "the incoming agent is a different program and cannot read the previous agent's conversation"},
			want: map[string]any{"carried": false, "carry_failure": "the incoming agent is a different program and cannot read the previous agent's conversation"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			oldCall := handoffSessionViaDaemon
			oldTo, oldBrief, oldAccount := sessionsHandoffTo, sessionsHandoffBrief, sessionsHandoffAccount
			oldRepo, oldEnvelope := repoFlag, envelopeOutput
			t.Cleanup(func() {
				handoffSessionViaDaemon = oldCall
				sessionsHandoffTo, sessionsHandoffBrief, sessionsHandoffAccount = oldTo, oldBrief, oldAccount
				repoFlag, envelopeOutput = oldRepo, oldEnvelope
			})
			sessionsHandoffTo, sessionsHandoffBrief, sessionsHandoffAccount = "claude", "", "work"
			repoFlag, envelopeOutput = "", false
			handoffSessionViaDaemon = func(daemon.HandoffSessionRequest) (daemon.HandoffSessionResponse, error) {
				return daemon.HandoffSessionResponse{OK: true, From: "claude", To: "claude", ToAccount: "work", Conversation: tc.conv}, nil
			}
			out := captureStdout(t, func() { require.NoError(t, sessionsHandoffCmd.RunE(sessionsHandoffCmd, []string{"worker"})) })
			var payload map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &payload))
			require.Equal(t, tc.want, payload["conversation"])
		})
	}
}

func TestHandoffCommittedErrorPrintsSuccessAndWarning(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	oldCall := handoffSessionViaDaemon
	oldTo, oldBrief, oldAccount := sessionsHandoffTo, sessionsHandoffBrief, sessionsHandoffAccount
	oldRepo, oldEnvelope := repoFlag, envelopeOutput
	t.Cleanup(func() {
		handoffSessionViaDaemon = oldCall
		sessionsHandoffTo, sessionsHandoffBrief, sessionsHandoffAccount = oldTo, oldBrief, oldAccount
		repoFlag, envelopeOutput = oldRepo, oldEnvelope
	})
	sessionsHandoffTo, sessionsHandoffBrief, sessionsHandoffAccount = "claude", "", "personal"
	repoFlag, envelopeOutput = "", false
	handoffSessionViaDaemon = func(daemon.HandoffSessionRequest) (daemon.HandoffSessionResponse, error) {
		return daemon.HandoffSessionResponse{OK: true, From: "claude", To: "claude", FromAccount: "work", ToAccount: "personal", HeadSHA: "abc123"}, committedHandoffError{}
	}
	// The message is supplied by the daemon in production; this fixture only
	// needs the committed classification to exercise the output branch.
	out := captureStdout(t, func() { require.NoError(t, sessionsHandoffCmd.RunE(sessionsHandoffCmd, []string{"worker"})) })
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &payload))
	require.Equal(t, true, payload["ok"])
	require.Equal(t, "claude", payload["from"])
	require.Equal(t, "personal", payload["to_account"])
	require.Contains(t, payload, "warning")
}
