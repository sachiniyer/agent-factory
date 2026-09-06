package apiproto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvelopeDaemonProvenanceRoundTrip(t *testing.T) {
	const body = `{"data":null,"error":{"message":"refused","daemon_rejected":true}}`
	var env Envelope
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	encoded, err := json.Marshal(env)
	require.NoError(t, err)
	require.JSONEq(t, body, string(encoded))
}
