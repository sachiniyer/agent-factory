package app

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHelpTitleWithAndWithoutVersion(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = ""
	require.Equal(t, "Agent Factory", helpProductTitle())
	Version = "1.0.280"
	require.Equal(t, "Agent Factory v1.0.280", helpProductTitle())
}
