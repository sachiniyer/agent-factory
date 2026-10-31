package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEditorOriginPorts(t *testing.T) {
	got, err := ParseEditorOriginPorts("9100-9102, 9105,9101")
	require.NoError(t, err)
	assert.Equal(t, []int{9100, 9101, 9102, 9105}, got)

	off, err := ParseEditorOriginPorts("  ")
	require.NoError(t, err)
	assert.Empty(t, off, "empty is the valid off value")

	for _, bad := range []string{"abc", "9200-9100", "80", "70000", "1024-9999", "9100-", "9100,,9101"} {
		_, err := ParseEditorOriginPorts(bad)
		assert.Error(t, err, bad)
	}
}

func TestEditorOriginPortsIsAValidatedGlobalKey(t *testing.T) {
	spec, ok := settableKeySpecs["network.editor_origin_ports"]
	require.True(t, ok)
	require.Error(t, spec.validate("network.editor_origin_ports", "9200-9100"))
	require.NoError(t, spec.validate("network.editor_origin_ports", "9100-9119"))
}
