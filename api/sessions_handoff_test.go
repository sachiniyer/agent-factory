package api

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/require"
)

func TestHandoffOutputOmitsAmbientAccounts(t *testing.T) {
	for _, tc := range []struct{ name, fromAccount, toAccount string }{
		{name: "agent-only"},
		{name: "ambient-to-pinned", toAccount: "personal"},
		{name: "pinned-to-pinned", fromAccount: "work", toAccount: "personal"},
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
			expected := map[string]any{"ok": true, "title": "worker", "from": "codex", "to": "claude", "head_sha": "branch-tip"}
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
