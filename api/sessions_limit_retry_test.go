package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

type committedLimitRetryError struct{ message string }

func (e committedLimitRetryError) Error() string         { return e.message }
func (committedLimitRetryError) MutationCommitted() bool { return true }

// TestSessionsRetryLimit_UsesSharedDaemonPath pins the surface-parity fix: the
// CLI must hand the selected session to the existing ResumeFromLimit daemon
// action, scoped exactly like the other title-addressed session mutations. The
// daemon owns every resume detail (respawn, pending-prompt delivery, and durable
// liveness clearing); the CLI must not grow a second implementation of them.
func TestSessionsRetryLimit_UsesSharedDaemonPath(t *testing.T) {
	repoID := setupRepoForCmd(t)

	var gotReq daemon.ResumeFromLimitRequest
	prev := resumeFromLimitViaDaemon
	resumeFromLimitViaDaemon = func(req daemon.ResumeFromLimitRequest) (daemon.ResumeFromLimitResponse, error) {
		gotReq = req
		return daemon.ResumeFromLimitResponse{OK: true}, nil
	}
	t.Cleanup(func() { resumeFromLimitViaDaemon = prev })

	out, err := runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	if err != nil {
		t.Fatalf("retry-limit returned error: %v", err)
	}
	if gotReq.Title != "worker" || gotReq.RepoID != repoID || gotReq.ID != "" {
		t.Fatalf("ResumeFromLimit request = %+v, want title %q scoped to repo %q with no web-only id", gotReq, "worker", repoID)
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("output is not JSON (%q): %v", out, err)
	}
	if payload["ok"] != true || payload["title"] != "worker" {
		t.Fatalf("JSON output = %v, want ok=true and title=worker", payload)
	}
}

// TestSessionsRetryLimit_SurfacesDaemonRejection keeps the CLI from claiming a
// retry happened when the shared daemon path says the target is not limit-
// blocked (or rejects it for any other reason).
func TestSessionsRetryLimit_SurfacesDaemonRejection(t *testing.T) {
	setupRepoForCmd(t)

	prev := resumeFromLimitViaDaemon
	resumeFromLimitViaDaemon = func(daemon.ResumeFromLimitRequest) (daemon.ResumeFromLimitResponse, error) {
		return daemon.ResumeFromLimitResponse{}, errors.New("session \"worker\" is not blocked on a usage limit")
	}
	t.Cleanup(func() { resumeFromLimitViaDaemon = prev })

	_, err := runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	if err == nil {
		t.Fatal("retry-limit must surface the daemon rejection")
	}
	if !strings.Contains(err.Error(), "not blocked on a usage limit") {
		t.Fatalf("error = %v, want the daemon's rejection message", err)
	}
}

func TestSessionsRetryLimit_PrintsCommittedSettlementWarningAsSuccess(t *testing.T) {
	setupRepoForCmd(t)

	prev := resumeFromLimitViaDaemon
	resumeFromLimitViaDaemon = func(daemon.ResumeFromLimitRequest) (daemon.ResumeFromLimitResponse, error) {
		return daemon.ResumeFromLimitResponse{OK: true}, committedLimitRetryError{message: "mission delivered; settlement pending"}
	}
	t.Cleanup(func() { resumeFromLimitViaDaemon = prev })

	out, err := runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	if err != nil {
		t.Fatalf("committed retry returned error: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("output is not JSON (%q): %v", out, err)
	}
	if payload["ok"] != true || payload["warning"] != "mission delivered; settlement pending" {
		t.Fatalf("JSON output = %v, want ok=true with committed warning", payload)
	}
}

// #6768: the retry output must hand the caller the conversation verdict the
// daemon computed — carried, or the stated fresh start — rather than ok:true
// alone, which previously hid a replacement that lost the transcript.
func TestSessionsRetryLimit_ReportsTheConversationOutcome(t *testing.T) {
	setupRepoForCmd(t)

	prev := resumeFromLimitViaDaemon
	resumeFromLimitViaDaemon = func(daemon.ResumeFromLimitRequest) (daemon.ResumeFromLimitResponse, error) {
		return daemon.ResumeFromLimitResponse{
			OK:           true,
			Conversation: session.HandoffConversation{CarryFailure: "af recorded no conversation id for this codex session"},
		}, nil
	}
	t.Cleanup(func() { resumeFromLimitViaDaemon = prev })

	out, err := runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(out, &payload))
	require.Equal(t,
		map[string]any{"carried": false, "carry_failure": "af recorded no conversation id for this codex session"},
		payload["conversation"])

	resumeFromLimitViaDaemon = func(daemon.ResumeFromLimitRequest) (daemon.ResumeFromLimitResponse, error) {
		return daemon.ResumeFromLimitResponse{
			OK:           true,
			Conversation: session.HandoffConversation{Carried: true},
		}, nil
	}
	out, err = runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(out, &payload))
	require.Equal(t, map[string]any{"carried": true}, payload["conversation"])
}

// #6768: --delivered retires the pending swap whose record held the carry
// verdict, so its response must answer the same question.
func TestSessionsRetryLimit_DeliveredReportsTheConversationOutcome(t *testing.T) {
	setupRepoForCmd(t)

	prev := confirmHandoffDeliveryViaDaemon
	confirmHandoffDeliveryViaDaemon = func(daemon.ConfirmHandoffDeliveryRequest) (daemon.ConfirmHandoffDeliveryResponse, error) {
		return daemon.ConfirmHandoffDeliveryResponse{
			OK:           true,
			Conversation: session.HandoffConversation{Carried: true},
		}, nil
	}
	t.Cleanup(func() { confirmHandoffDeliveryViaDaemon = prev })
	oldDelivered := sessionsRetryLimitDelivered
	sessionsRetryLimitDelivered = true
	t.Cleanup(func() { sessionsRetryLimitDelivered = oldDelivered })

	out, err := runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(out, &payload))
	require.Equal(t, true, payload["delivered"])
	require.Equal(t, map[string]any{"carried": true}, payload["conversation"])
}
