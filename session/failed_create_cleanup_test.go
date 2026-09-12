package session

import (
	"errors"
	"fmt"
	"testing"

	gitworktree "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failedCreateCleanupBackend struct {
	*FakeBackend
	killCalls int
}

func (b *failedCreateCleanupBackend) Kill(instance *Instance, _ bool) error {
	b.killCalls++
	return b.FakeBackend.Kill(instance, false)
}

func TestCleanupFailedCreateHonorsSetupOwnershipRefusal(t *testing.T) {
	backend := &failedCreateCleanupBackend{FakeBackend: NewFakeBackend()}
	instance := &Instance{backend: backend}
	refusal := fmt.Errorf("launch failed: %w", gitworktree.ErrSetupRemovalOwnershipUnproven)

	require.NoError(t, instance.CleanupFailedCreate(refusal))
	assert.Zero(t, backend.killCalls,
		"a create that made no workspace must not turn setup's refusal into kill authority")

	require.NoError(t, instance.CleanupFailedCreate(errors.New("agent failed to start")))
	assert.Equal(t, 1, backend.killCalls,
		"other failed creates still owe ordinary resource cleanup")
}
