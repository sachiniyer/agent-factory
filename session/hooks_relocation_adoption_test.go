//go:build linux

package session

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/require"
)

func TestRestoredRelocationRecoveryBlocksHookAdoption(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	data := hookScopeInstanceData(t, "af-hook-recovery")
	data.Liveness = LiveLost
	original := false
	created := true
	data.Worktree.RelocationRecovery = &GitWorktreeRelocationRecoveryData{
		State:                       git.RelocationRecoveryStalled,
		OriginalExternalWorktree:    &original,
		OriginalBranchCreatedByUs:   &created,
		OriginalStartupStateUnknown: &original,
	}
	inst, err := FromInstanceData(data)
	require.NoError(t, err)
	require.True(t, inst.gitWorktree.HasUnresolvedRelocation())
	dir := t.TempDir()
	marker := filepath.Join(dir, "probed")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 0\n"), 0700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	git.AdoptRunningHooks([]*git.GitWorktree{inst.gitWorktree})
	done := inst.gitWorktree.HooksDone()
	require.NotNil(t, done, "unresolved relocation must report its pending hook obligation")
	t.Cleanup(func() { require.NoError(t, inst.gitWorktree.CancelAndJoinHooks()) })
	select {
	case <-done:
		t.Fatal("unresolved relocation reported its hook obligation complete")
	case <-time.After(100 * time.Millisecond):
	}
	_, err = os.Stat(marker)
	require.True(t, os.IsNotExist(err), "unresolved relocation must be fenced before probing/adopting legacy scopes")
}
