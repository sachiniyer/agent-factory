package session

import (
	"reflect"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

func TestHookCreateHoldArmsWorktreeProvisionedAfterHold(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := initInPlaceRepo(t, "main")
	cfg := config.DefaultConfig()
	require.NoError(t, config.SaveConfig(cfg))
	prefix := ""
	instance, err := NewInstance(InstanceOptions{
		Title: "hook-create-hold", Path: repo, Program: "claude", Backend: BackendLocal, BranchPrefix: &prefix,
	})
	require.NoError(t, err)
	release := instance.HoldHookProgressUntilCreateSettled()
	if !instance.hookCreatePersistencePending {
		t.Fatal("create hold was a no-op before worktree provisioning")
	}
	require.NoError(t, instance.backend.Provision(instance, true))
	instance.mu.RLock()
	worktree := instance.gitWorktree
	instance.mu.RUnlock()
	if worktree == nil {
		t.Fatal("normal local provisioning did not assign a worktree")
	}
	if !reflect.ValueOf(worktree).Elem().FieldByName("hookCreatePending").Bool() {
		t.Fatal("normal local worktree did not observably inherit the create hold")
	}
	release()
	if instance.hookCreatePersistencePending || reflect.ValueOf(worktree).Elem().FieldByName("hookCreatePending").Bool() {
		t.Fatal("settled create retained its pre-commit hold")
	}
}
