package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Finding 3687787045 (backend_hook_remote.go:266): an unquoted property name in a
// malformed structured log leaves the parent state invalid, so the nested
// endpoint-shaped value is promoted over the real launch record on stdout.
func TestHookProvisionRejectsUnquotedEndpointPropertyInMalformedLog(t *testing.T) {
	h := newHookState(t, `
echo '{"level":INVALID, endpoint:{"url":"http://property.invalid","token":"property-secret"}}' >&2
echo '{"url":"http://10.0.0.7:8080","token":"secret"}'
exit 0
`, "")
	p := newHookProvisioner(h, "unquoted property logger")

	res, err := p.provisionOrReap()
	require.NoError(t, err, "an unquoted log property must not hide the launch record")
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, "http://10.0.0.7:8080", res.Endpoint.URL)
	assert.Equal(t, "secret", res.Endpoint.Token)
	assert.False(t, h.deleteRan(t), "valid endpoint output must not reap the working sandbox")
}
