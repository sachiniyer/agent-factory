package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetGlobalConfigValueIf_HostsCompareAndSet(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_, err := LoadConfig()
	require.NoError(t, err)

	empty := HostsSnapshot(nil)
	_, _, err = SetGlobalConfigValueIfWithDigest("hosts", `{"a":{"host":"a.example"}}`, &empty)
	require.NoError(t, err, "the table is still empty, so the write lands")

	// A second writer that read the empty table is stale now.
	_, _, err = SetGlobalConfigValueIfWithDigest("hosts", `{"b":{"host":"b.example"}}`, &empty)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), ErrConfigChangedConcurrently))
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Contains(t, cfg.Hosts, "a", "the stale write changed nothing")
	assert.NotContains(t, cfg.Hosts, "b")

	current := HostsSnapshot(cfg.Hosts)
	_, _, err = SetGlobalConfigValueIfWithDigest("hosts", `{"a":{"host":"a.example"},"b":{"host":"b.example"}}`, &current)
	require.NoError(t, err)

	_, _, err = SetGlobalConfigValueIfWithDigest("default_program", "claude", &current)
	require.Error(t, err, "a conditional write is only defined for hosts")
}
