package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Absolute interpreter and output paths keep the stand-in working with PATH
// restricted to this directory; no real tmux can be invoked by these tests.
func fakeIdentityTmux(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	t.Setenv("IDENTITY_CALLS", calls)
	t.Setenv("IDENTITY_ANSWER", "af_other")
	t.Setenv("IDENTITY_EXIT", "0")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$IDENTITY_CALLS\"\nprintf '%s\\n' \"$IDENTITY_ANSWER\"\nexit \"$IDENTITY_EXIT\"\n"), 0o755))
	t.Setenv("PATH", dir)
	t.Setenv("TMUX", "/tmp/identity-socket,123,0")
	t.Setenv("TMUX_PANE", "%42")
	t.Setenv("AF_SESSION", "")
	t.Setenv("AF_SESSION_GEN", "parent-generation")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))
	return calls
}

func TestCurrentTmuxName(t *testing.T) {
	for _, tc := range []struct {
		name, tmux, pane, marker, answer, exit, wantErr string
		noCall                                          bool
		legacy                                          bool
	}{
		{name: "outside", pane: "%42", answer: "af_other", wantErr: "not running inside a tmux session", noCall: true},
		{name: "outside with stale marker", marker: "af_other", answer: "af_other", wantErr: "not running inside a tmux session", noCall: true},
		{name: "target pane", tmux: "/tmp/custom,socket,123,0", pane: "%42", answer: "af_other"},
		{name: "inherited stale marker", tmux: "/tmp/identity-socket,123,0", pane: "%42", marker: "af_parent", answer: "af_other", legacy: true},
		{name: "matching marker", tmux: "/tmp/identity-socket,123,0", pane: "%42", marker: "af_other", answer: "af_other"},
		{name: "mismatched marker", tmux: "/tmp/identity-socket,123,0", pane: "%42", marker: "af_me", answer: "af_other", wantErr: "AF_SESSION"},
		{name: "missing pane", tmux: "/tmp/identity-socket,123,0", answer: "af_other", wantErr: "TMUX_PANE", noCall: true},
		{name: "invalid pane", tmux: "/tmp/identity-socket,123,0", pane: "other", answer: "af_other", wantErr: "TMUX_PANE", noCall: true},
		{name: "malformed socket context", tmux: "invalid", pane: "%42", answer: "af_other", wantErr: "TMUX socket", noCall: true},
		{name: "relative socket", tmux: "relative,123,0", pane: "%42", answer: "af_other", wantErr: "TMUX socket", noCall: true},
		{name: "empty answer", tmux: "/tmp/identity-socket,123,0", pane: "%42", answer: "  ", wantErr: "could not determine tmux session name"},
		{name: "query fails", tmux: "/tmp/identity-socket,123,0", pane: "%42", marker: "af_other", answer: "af_other", exit: "1", wantErr: "not running inside a tmux session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakeIdentityTmux(t)
			if tc.legacy {
				t.Cleanup(tmux.SetNewSessionEnvSupportForTest(false))
			}
			t.Setenv("TMUX", tc.tmux)
			if tc.tmux == "" {
				require.NoError(t, os.Unsetenv("TMUX"))
			}
			t.Setenv("TMUX_PANE", tc.pane)
			t.Setenv("AF_SESSION", tc.marker)
			t.Setenv("IDENTITY_ANSWER", tc.answer)
			if tc.exit != "" {
				t.Setenv("IDENTITY_EXIT", tc.exit)
			}
			name, err := currentTmuxName()
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				assert.Empty(t, name)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "af_other", name)
				argv, readErr := os.ReadFile(calls)
				require.NoError(t, readErr)
				socket := "/tmp/identity-socket"
				if tc.name == "target pane" {
					socket = "/tmp/custom,socket"
				}
				assert.Equal(t, "-S\n"+socket+"\ndisplay-message\n-p\n-t\n%42\n#{session_name}\n", string(argv))
			}
			if tc.noCall {
				_, statErr := os.Stat(calls)
				assert.True(t, os.IsNotExist(statErr), "tmux must not run")
			}
		})
	}
}

func TestSessionsArchiveSelfOutsideTmuxDoesNotTouchSessions(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	calls := fakeIdentityTmux(t)
	require.NoError(t, os.Unsetenv("TMUX"))
	var snapshots, archives int
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		snapshots++
		return []session.InstanceData{{ID: "unrelated-id", Title: "other", TmuxName: "af_other"}}, nil
	})
	previous := archiveSessionViaDaemon
	archiveSessionViaDaemon = func(daemon.ArchiveSessionRequest) (string, error) { archives++; return "/archived/other", nil }
	t.Cleanup(func() { archiveSessionViaDaemon = previous })
	sessionsArchiveSelf = true
	t.Cleanup(func() { sessionsArchiveSelf = false })
	_, err := runCmdCaptureStdout(t, sessionsArchiveCmd, nil)
	assert.ErrorContains(t, err, "--self must be run from inside an af session")
	assert.Zero(t, snapshots, "must refuse before reading sessions")
	assert.Zero(t, archives, "must never archive the unrelated session")
	_, statErr := os.Stat(calls)
	assert.True(t, os.IsNotExist(statErr), "tmux must not run")
}
