package api

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session"
)

// TestSessionsCreate_WhitespaceAccountIsTrimmedBeforeDaemonRequest verifies the
// CLI trims --account before building the daemon request, so a whitespace-only
// value is sent as "" (no account) and the skew check does not false-positive
// when the daemon stores the trimmed empty value. Before the fix, the raw
// whitespace string was sent verbatim, the daemon's TrimSpace gates treated it
// as absent, and the record stored it raw — while the CLI's skew check compared
// the raw sent value against the daemon's empty stored value and passed (since
// both were the same raw string), leaving the split-brain undetected.
func TestSessionsCreate_WhitespaceAccountIsTrimmedBeforeDaemonRequest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	silenceStdio(t)

	repo := t.TempDir()
	require.NoError(t, exec.Command("git", "init", repo).Run())

	// Capture the request the CLI sends to the daemon.
	var captured daemon.CreateSessionRequest
	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		captured = req
		return &session.InstanceData{Title: req.Title, Account: req.Account}, nil
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })

	setSessionsCreateFlags(t, "ws-cli-create", repo, false, false)
	prevAccount := createAccountFlag
	createAccountFlag = "   "
	t.Cleanup(func() { createAccountFlag = prevAccount })

	require.NoError(t, sessionsCreateCmd.RunE(sessionsCreateCmd, nil),
		"a whitespace-only account must be admitted as ambient, not refused")
	assert.Empty(t, captured.Account,
		"the CLI must trim a whitespace-only account to empty before sending to the daemon")
}

// TestSessionsCreate_ValidAccountWithSurroundingWhitespaceIsTrimmed verifies the
// CLI trims surrounding whitespace from a valid account name before building the
// daemon request, so the recorded Account is the trimmed name and the skew check
// compares trimmed-sent against trimmed-stored.
func TestSessionsCreate_ValidAccountWithSurroundingWhitespaceIsTrimmed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	silenceStdio(t)

	repo := t.TempDir()
	require.NoError(t, exec.Command("git", "init", repo).Run())

	// Register the account so the localCreateInputs pre-check passes.
	cfgHome, err := config.GetConfigDir()
	require.NoError(t, err)
	_, err = agentaccount.Register(cfgHome, "claude", "work")
	require.NoError(t, err)

	// Capture the request and return the trimmed account so the skew check passes.
	var captured daemon.CreateSessionRequest
	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		captured = req
		return &session.InstanceData{Title: req.Title, Account: req.Account}, nil
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })

	setSessionsCreateFlags(t, "ws-cli-trim", repo, false, false)
	prevProgram, prevAccount := createProgramFlag, createAccountFlag
	createProgramFlag, createAccountFlag = "claude", "  work  "
	t.Cleanup(func() { createProgramFlag, createAccountFlag = prevProgram, prevAccount })

	require.NoError(t, sessionsCreateCmd.RunE(sessionsCreateCmd, nil),
		"a valid account with surrounding whitespace must be admitted (as trimmed)")
	assert.Equal(t, "work", captured.Account,
		"the CLI must trim surrounding whitespace before sending to the daemon")
}

// TestLocalCreateInputs_WhitespaceAccountSkipsPreCheck verifies the local
// pre-check in localCreateInputs treats a whitespace-only --account as absent,
// rather than reaching agentaccount.Selected — which trims internally and
// silently returns a zero Account for whitespace, skipping the validation a
// non-empty value must pass.
func TestLocalCreateInputs_WhitespaceAccountSkipsPreCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	repo := t.TempDir()
	require.NoError(t, exec.Command("git", "init", repo).Run())

	setSessionsCreateFlags(t, "ws-precheck", repo, false, false)
	prevAccount := createAccountFlag
	createAccountFlag = "   "
	t.Cleanup(func() { createAccountFlag = prevAccount })

	// A whitespace-only account must not reach agentaccount.Selected (which
	// would silently succeed); it must be treated as "no account" and skip the
	// pre-check entirely. The function should return without error.
	_, _, err := localCreateInputs("ws-precheck", false)
	require.NoError(t, err,
		"a whitespace-only account must be treated as absent by localCreateInputs")
}
