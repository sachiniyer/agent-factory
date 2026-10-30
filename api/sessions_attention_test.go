package api

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
)

func stubSetAttention(t *testing.T) *[]daemon.SetAttentionRequest {
	t.Helper()
	var reqs []daemon.SetAttentionRequest
	prev := setAttentionViaDaemon
	setAttentionViaDaemon = func(req daemon.SetAttentionRequest) (daemon.SetAttentionResponse, error) {
		reqs = append(reqs, req)
		return daemon.SetAttentionResponse{ID: "id-1", Title: req.Title}, nil
	}
	t.Cleanup(func() { setAttentionViaDaemon = prev })
	return &reqs
}

// The attention request goes to the targeted daemon, so a titled target is scoped
// like the other routed session verbs: by the cwd repo for a local daemon, and not
// at all for a remote one, whose repos the client's cwd says nothing about.
func TestSessionsAttention_ScopesTheTitleLikeOtherRoutedVerbs(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := t.TempDir()
	require.NoError(t, exec.Command("git", "init", repo).Run())
	t.Chdir(repo)

	reqs := stubSetAttention(t)
	require.NoError(t, runWithFlags(t, newSessionsAttentionCmd(), map[string]string{"clear": "true"}, "peer"))
	require.Len(t, *reqs, 1)
	require.NotEmpty(t, (*reqs)[0].RepoID, "a local daemon is still scoped by the cwd repo")

	remoteTarget(t)
	*reqs = nil
	require.NoError(t, runWithFlags(t, newSessionsAttentionCmd(), map[string]string{"clear": "true"}, "peer"))
	require.Len(t, *reqs, 1)
	require.Equal(t, "peer", (*reqs)[0].Title)
	require.Empty(t, (*reqs)[0].RepoID, "a remote daemon must not be scoped by the client's cwd repo")
}
