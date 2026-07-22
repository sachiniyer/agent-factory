package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKillConfirmationWarningUsesSentenceCaseAtCompactSizes(t *testing.T) {
	cases := []struct {
		name     string
		prepare  func(*testing.T, string)
		expected []string
	}{
		{
			name: "dirty worktree",
			prepare: func(t *testing.T, wt string) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(wt, "untracked.txt"), []byte("work\n"), 0o644))
			},
			expected: []string{"Warning: This worktree has uncommitted changes that will be lost!"},
		},
		{
			name: "unverifiable worktree",
			prepare: func(t *testing.T, wt string) {
				t.Helper()
				indexPath := killGit(t, wt, "rev-parse", "--git-path", "index")
				if !filepath.IsAbs(indexPath) {
					indexPath = filepath.Join(wt, indexPath)
				}
				require.NoError(t, os.WriteFile(indexPath, []byte("broken"), 0o644))
			},
			expected: []string{
				"Warning: Could not verify worktree status",
				"it may contain uncommitted changes that will be lost!",
			},
		},
	}

	for _, size := range []struct {
		width  int
		height int
	}{
		{width: 80, height: 24},
		{width: 72, height: 20},
	} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s-%dx%d", tc.name, size.width, size.height), func(t *testing.T) {
				repoDir, baseSHA := initBaseRepo(t)
				wt := addWorktree(t, repoDir, baseSHA, "dev/kill-copy")
				tc.prepare(t, wt)
				inst := startedWorktreeInstance(t, "kill-copy", repoDir, wt, "dev/kill-copy", baseSHA)

				h := newTestHome(t)
				h.store.AddInstance(inst)
				h.sidebar.SetSelectedInstance(0)
				_ = h.selectionChanged()
				resizeHome(h, size.width, size.height)

				killKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")}
				_, _ = h.Update(killKey)
				_, _ = h.Update(killKey)
				require.Equal(t, stateConfirm, h.state, "D opens the production kill confirmation")
				require.NotNil(t, h.confirmationOverlay)

				view := h.View()
				requireViewSized(t, view, size.width, size.height)
				copy := flatten(view)
				for _, expected := range tc.expected {
					assert.Contains(t, copy, expected,
						"%dx%d: the data-loss warning follows the TUI sentence-case convention", size.width, size.height)
				}
				assert.NotContains(t, copy, "WARNING:",
					"%dx%d: the data-loss warning must not caps-shout", size.width, size.height)
			})
		}
	}
}
