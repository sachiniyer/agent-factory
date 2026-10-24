package apiproto

import (
	"bytes"
	"errors"
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

// TestEnvelope_BoundedProbeNoAnswerCode locks the wire contract for the
// bounded-probe-non-answer class (#5295 review): the code constant is
// "bounded_probe_no_answer", a failure carrying it round-trips through the
// envelope with exactly that code, and the client-side sentinel
// ErrRemoteBoundedProbeNoAnswer is a non-nil error identity
// errors.Is-based policy can match — the same shape the sibling
// ErrorCodeResourceExhausted/ErrRemoteResourceExhausted pair already relies on
// for the errno class crossing the wire.
func TestEnvelope_BoundedProbeNoAnswerCode(t *testing.T) {
	require.Equal(t, "bounded_probe_no_answer", ErrorCodeBoundedProbeNoAnswer)
	require.NotNil(t, ErrRemoteBoundedProbeNoAnswer,
		"ErrRemoteBoundedProbeNoAnswer must be a non-nil errors.New sentinel")
	require.True(t, errors.Is(ErrRemoteBoundedProbeNoAnswer, ErrRemoteBoundedProbeNoAnswer),
		"the sentinel must be its own errors.Is identity")

	var buf bytes.Buffer
	require.NoError(t, WriteEnvelope(&buf, FailureWithCode("probe did not answer", ErrorCodeBoundedProbeNoAnswer)))
	require.Equal(t,
		"{\n  \"data\": null,\n  \"error\": {\n    \"message\": \"probe did not answer\",\n    \"code\": \"bounded_probe_no_answer\"\n  }\n}\n",
		buf.String())
}

// TestEnvelope_DistinctDeadlineCodes pins that the two deadline-class wire
// codes are distinct strings: the bare-deadline code (preserve push's own
// network deadline, structural on the Recover path) and the
// bounded-probe-non-answer code (a deadline paired with a sentinel,
// environmental on both paths) carry different classes, so a client must be
// able to tell them apart (#5295 review).
func TestEnvelope_DistinctDeadlineCodes(t *testing.T) {
	require.NotEqual(t, ErrorCodeDeadlineExceeded, ErrorCodeBoundedProbeNoAnswer)
	require.NotEqual(t, ErrorCodeResourceExhausted, ErrorCodeBoundedProbeNoAnswer)
	require.NotEqual(t, ErrRemoteResourceExhausted, ErrRemoteBoundedProbeNoAnswer)
}
