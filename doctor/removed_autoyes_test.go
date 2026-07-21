package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
)

func TestDoctorAcceptsConfigWithRemovedAutoYesDuringUpgrade(t *testing.T) {
	testguard.IsolateTmux(t)
	opts := testOptions(t, false)
	require.NoError(t, os.WriteFile(
		filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("auto_yes = true\n"), 0o600))

	report, err := Run(opts)
	require.NoError(t, err)
	rows := findCheckRows(report, "config")
	require.Len(t, rows, 1)
	require.Equal(t, StatusPass, rows[0].Status)
	require.Contains(t, rows[0].Detail, "loaded")
}
