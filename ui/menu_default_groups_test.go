package ui

import (
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDefaultMenuGroupsMatchActions(t *testing.T) {
	for _, state := range []MenuState{StateEmpty, StateDefault} {
		m := NewMenu()
		m.SetState(state)
		require.Len(t, m.groups, 3)
		want := [][]keys.KeyName{{keys.KeyNew}, {keys.KeySearch}, {keys.KeyHelp, keys.KeyQuit}}
		for i, group := range m.groups {
			require.LessOrEqual(t, group.end, len(m.options))
			require.Equal(t, want[i], m.options[group.start:group.end])
			require.Equal(t, i == 0, group.isAction)
		}
	}
}
