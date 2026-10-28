package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stubHostPickSupport(t *testing.T, supported bool, version string, err error) {
	t.Helper()
	prev := hostPickSupportViaDaemon
	hostPickSupportViaDaemon = func() (bool, string, error) { return supported, version, err }
	t.Cleanup(func() { hostPickSupportViaDaemon = prev })
}

// A daemon that does not affirm host picks must never receive one: it would
// create the session elsewhere and the CLI would report success (#5746).
func TestRequireHostPickSupportRefusesAnOlderDaemon(t *testing.T) {
	stubHostPickSupport(t, false, "1.0.280", nil)
	err := requireHostPickSupport("box")
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "daemon (v1.0.280) does not support --host")
	assert.Contains(t, msg, `"box"`)
	assert.Contains(t, msg, "af daemon restart")
	assert.Contains(t, msg, "nothing was created")

	stubHostPickSupport(t, false, "", nil)
	err = requireHostPickSupport("box")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "predates version reporting"))
}

func TestRequireHostPickSupportFailsClosedWhenUnverifiable(t *testing.T) {
	stubHostPickSupport(t, false, "", errors.New("connection refused"))
	err := requireHostPickSupport("box")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "was not sent")
	assert.Contains(t, err.Error(), "connection refused")
}

func TestRequireHostPickSupportAllowsACapableDaemon(t *testing.T) {
	stubHostPickSupport(t, true, "1.0.300", nil)
	require.NoError(t, requireHostPickSupport("box"))
}
