package tree

import (
	"github.com/sachiniyer/agent-factory/ui/theme"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestArchiveFailureDoesNotReuseLostRole(t *testing.T) {
	require.Equal(t, theme.Roles().Dead, archiveWarningColor)
	require.Equal(t, theme.Roles().Lost, lostStyle.GetForeground())
}
