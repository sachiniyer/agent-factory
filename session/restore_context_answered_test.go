package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// #7088 review, P3: a snapshot answers every read of the batch with the one
// answer it got — an invalid project config included. Re-reading it per caller
// asked git again with a load's single attempt, so a config that parses but
// holds an invalid value could turn into an unknown, and a refusal, under load.
func TestRestoreContextRemembersAnAnsweredConfigError(t *testing.T) {
	rc := NewRestoreContext()
	rc.attempts = 1
	repo := &config.RepoContext{Root: "/repo", ID: "repo-id"}
	rc.repoFromPath = func(string) (*config.RepoContext, error) { return repo, nil }
	invalid := errors.New("project config: default_program: unknown agent")
	calls := 0
	rc.resolveConfig = func(*config.RepoContext) (*config.ResolvedConfig, error) {
		calls++
		return nil, invalid
	}

	for n := 0; n < 3; n++ {
		_, err := rc.ResolvedConfigFor("/repo")
		require.ErrorIs(t, err, invalid)
	}
	require.Equal(t, 1, calls, "the answer is read once and given to every caller")
}

// Control: a read that went unanswered is not an answer, and the next caller
// asks again.
func TestRestoreContextAsksAgainAfterAnUnansweredRead(t *testing.T) {
	rc := NewRestoreContext()
	rc.attempts = 1
	repo := &config.RepoContext{Root: "/repo", ID: "repo-id"}
	rc.repoFromPath = func(string) (*config.RepoContext, error) { return repo, nil }
	calls := 0
	rc.resolveConfig = func(*config.RepoContext) (*config.ResolvedConfig, error) {
		calls++
		return nil, fmt.Errorf("read checkout marker: %w", context.DeadlineExceeded)
	}

	for n := 0; n < 2; n++ {
		_, err := rc.ResolvedConfigFor("/repo")
		require.True(t, IsProjectConfigUnknown(err), "%v", err)
	}
	require.Equal(t, 2, calls)
}
