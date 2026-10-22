package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func TestIsTypedInputIgnoresTerminalReplies(t *testing.T) {
	cases := map[string]bool{
		"a":                        true,
		"\r":                       true,
		"\x03":                     true,
		"\x1b[I":                   false,
		"\x1b[O":                   false,
		"\x1b[12;1R":               false,
		"":                         false,
		"\x1b[200~yes\x1b[201~":    true,
		"\x1b[200~line one\nline ": true,
		"\x1b[200~":                false,
		"\x1b[200~\x1b[201~":       false,
	}
	for in, want := range cases {
		if got := isTypedInput([]byte(in)); got != want {
			t.Errorf("isTypedInput(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestClearAttentionOnInputDoesNotWaitForTheRepoStartLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, "asker", "af_attention_input")
	_, err = inst.SetAttention("pick one", time.Unix(100, 0))
	require.NoError(t, err)

	var pending []func()
	prev := runAttentionClear
	runAttentionClear = func(clear func()) { pending = append(pending, clear) }
	t.Cleanup(func() { runAttentionClear = prev })

	lock := manager.startLockForRepo(repo.ID)
	lock.Lock()
	manager.clearAttentionOnInput(inst)
	manager.clearAttentionOnInput(inst)
	lock.Unlock()
	require.Len(t, pending, 1, "a burst of keys starts one clear")

	_, err = inst.SetAttention("a newer question", time.Unix(200, 0))
	require.NoError(t, err)
	pending[0]()
	require.True(t, inst.HasAttention(), "a request set after the keystroke stays")

	manager.clearAttentionOnInput(inst)
	require.Len(t, pending, 2, "the finished clear no longer blocks the next one")
	pending[1]()
	require.False(t, inst.HasAttention())
}
