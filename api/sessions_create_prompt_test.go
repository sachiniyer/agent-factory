package api

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// Only the CLI runs: preflight and create are stubbed, so this never starts a
// daemon, agent, or tmux session. The returned evidence models the real sender.
func TestSessionsCreateReportsPromptUncertainty(t *testing.T) {
	for _, envelope := range []bool{false, true} {
		mode := "plain"
		if envelope {
			mode = "envelope"
		}
		t.Run(mode, func(t *testing.T) {
			previousEnvelope := envelopeOutput
			previousWarnWriter := warnWriter
			envelopeOutput = envelope
			t.Cleanup(func() {
				envelopeOutput = previousEnvelope
				warnWriter = previousWarnWriter
			})
			for _, tc := range []struct {
				name        string
				status      session.PromptDeliveryStatus
				prompt      string
				parked      bool
				wantWarning bool
			}{
				{"collapsed_paste", session.PromptSentUnverified, "do work", false, true},
				{"observed_absent", session.PromptNotDelivered, "do work", false, true},
				{"unreadable", session.PromptCouldNotConfirm, "do work", false, true},
				{"older_daemon", "", "do work", false, true},
				{"future_status", "future", "do work", false, true},
				{"observed_delivery", session.PromptDelivered, "do work", false, false},
				{"no_prompt", "", "", false, false},
				{"limit_parked", "", "do work", true, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var warningOutput bytes.Buffer
					warnWriter = &warningOutput
					t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
					repo := t.TempDir()
					require.NoError(t, exec.Command("git", "init", repo).Run())
					setSessionsCreateFlags(t, "prompt-lane", repo, false, false)
					createPromptFlag = tc.prompt
					original := createSessionViaDaemon
					calls := 0
					createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
						calls++
						require.Equal(t, tc.prompt, req.Prompt)
						data := &session.InstanceData{ID: "stable-id", Title: req.Title, LastPromptDeliveryStatus: tc.status}
						if tc.parked {
							data.Liveness = session.LiveLimitReached
						}
						return data, nil
					}
					t.Cleanup(func() { createSessionViaDaemon = original })
					out, runErr := runCmdCaptureStdout(t, sessionsCreateCmd, nil)
					require.NoError(t, runErr, "creation succeeded; uncertainty must not invite blind recreation")
					require.Equal(t, 1, calls)
					var got struct {
						session.InstanceData
						Warning string `json:"warning"`
					}
					if envelope {
						var wrapped struct {
							Error *json.RawMessage `json:"error"`
							Data  json.RawMessage  `json:"data"`
						}
						require.NoError(t, json.Unmarshal(out, &wrapped))
						require.Nil(t, wrapped.Error)
						out = wrapped.Data
					}
					require.NoError(t, json.Unmarshal(out, &got))
					var projected map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(out, &projected))
					require.Contains(t, projected, "status_name")
					require.Contains(t, projected, "liveness_name")
					require.Equal(t, "stable-id", got.ID)
					require.Equal(t, tc.status, got.LastPromptDeliveryStatus)
					diagnostic := warningOutput.String()
					if envelope {
						require.Empty(t, diagnostic, "--json must emit only its structured envelope")
					}
					if tc.wantWarning {
						require.NotEmpty(t, got.Warning)
						require.Contains(t, got.Warning, "created")
						require.Contains(t, got.Warning, "prompt")
						require.Contains(t, got.Warning, "Inspect")
						if !envelope {
							require.Contains(t, diagnostic, got.Warning)
						}
					} else {
						require.Empty(t, got.Warning)
						require.False(t, strings.Contains(diagnostic, "Warning:"))
					}
					require.Less(t, strings.Index(string(out), `"title"`), strings.Index(string(out), `"path"`), "adding warning must preserve InstanceData's public field order")
				})
			}

		})
	}
}
