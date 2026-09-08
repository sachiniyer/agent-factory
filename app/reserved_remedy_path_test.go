package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

func TestReservedTitleRemedyUsesNamingRepo(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew
	repo := t.TempDir()
	h.namingInstance = &session.Instance{Title: "root", Path: repo}
	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})
	text := h.errBox.FullError()
	require.Contains(t, text, "af projects add "+config.ShellQuotePath(repo))
	require.Contains(t, text, "af config set --project "+config.ShellQuotePath(repo))
}
