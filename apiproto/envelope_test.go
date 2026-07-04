package apiproto

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests lock the EXACT wire bytes of the shared envelope. Both the `af`
// CLI (--json) and the daemon HTTP server encode through WriteEnvelope, so
// pinning the shape here guarantees the two surfaces stay identical: a change
// that would drift the HTTP body from the CLI output breaks this test.

func TestWriteEnvelope_SuccessExactBytes(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteEnvelope(&buf, Success(map[string]bool{"ok": true})))
	require.Equal(t,
		"{\n  \"data\": {\n    \"ok\": true\n  },\n  \"error\": null\n}\n",
		buf.String())
}

func TestWriteEnvelope_FailureExactBytes(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteEnvelope(&buf, Failure("boom")))
	require.Equal(t,
		"{\n  \"data\": null,\n  \"error\": {\n    \"message\": \"boom\"\n  }\n}\n",
		buf.String())
}
