package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #5744: validateCreateFirst refuses an unregistered explicit account before any
// reservation could run, so a refused remote create leaves no partial state.
func TestValidateCreateFirstRefusesUnknownAccount(t *testing.T) {
	_, repoPath, _ := defaultAccountFixture(t, "codex", "work")
	err := validateCreateFirst(CreateSessionRequest{
		Title: "t", RepoPath: repoPath, Program: "codex", Account: "no-such-account", Backend: "docker",
	})
	require.Error(t, err)
}
