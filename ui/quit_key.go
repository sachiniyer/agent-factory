package ui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/keys"
)

func configuredQuitKey(msg tea.KeyMsg) bool {
	return key.Matches(msg, keys.GlobalKeyBindings[keys.KeyQuit])
}
