package layout_test

import (
	"testing"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/require"
)

func TestDesignCutsKeepSingleProjectOutOfRail(t *testing.T) {
	for _, projects := range []int{0, 1, 2} {
		got := (layout.Grid{Panes: 1, Projects: projects}).Solve(120, 36)
		require.False(t, got.AutomationsVisible)
		require.Equal(t, projects > 1, got.ProjectsVisible)
		if projects <= 1 {
			require.Equal(t, 36-layout.StatusBarRows, got.Tree.H)
		}
	}
}
