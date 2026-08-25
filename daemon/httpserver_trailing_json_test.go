package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeRequestBody runs one body through decodeHTTPRequest as a given
// population: clientVersion empty is hand-authored (curl, `af api`, a script),
// which is the strict-decoding population, and non-empty is an af client.
func decodeRequestBody(t *testing.T, body, clientVersion string, dst any) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/Snapshot", strings.NewReader(body))
	if clientVersion != "" {
		req.Header.Set(agentproto.ClientVersionHeader, clientVersion)
	}
	return decodeHTTPRequest(httptest.NewRecorder(), req, dst)
}

// TestDecodeHTTPRequest_ConcatenatedJSONNamesTheTrailingValue is #3406.
//
// decodeHTTPRequest decodes a second time to detect trailing JSON. Strict mode
// is a property of the DECODER rather than of one Decode call, so on the
// hand-authored path that second decode ran with DisallowUnknownFields too and
// reported `unknown field "repo_id"` — about a field that exists and is spelled
// correctly. The reader is sent hunting a typo that is not there, and the one
// thing wrong with their request is never mentioned.
//
// Diagnosability is the whole fix: the request is rejected either way.
func TestDecodeHTTPRequest_ConcatenatedJSONNamesTheTrailingValue(t *testing.T) {
	bodies := map[string]string{
		"two objects":            `{"repo_id":"a"}{"repo_id":"b"}`,
		"object then scalar":     `{"repo_id":"a"} 5`,
		"newline-separated":      "{\"repo_id\":\"a\"}\n{\"repo_id\":\"b\"}",
		"object then empty list": `{"repo_id":"a"}[]`,
	}
	// Both populations, because the same body used to produce two different
	// diagnoses depending on a header documented as opting out of TYPO checking —
	// which is not a difference in what is wrong with the request.
	for _, clientVersion := range []string{"", "9.9.9"} {
		population := "hand-authored"
		if clientVersion != "" {
			population = "af-client"
		}
		for name, body := range bodies {
			t.Run(population+"/"+name, func(t *testing.T) {
				var req SnapshotRequest
				err := decodeRequestBody(t, body, clientVersion, &req)

				require.Error(t, err, "a body carrying more than one JSON value must be rejected")
				assert.Contains(t, err.Error(), "multiple JSON values",
					"the error must name the real fault: there is a second value after the first")
				assert.NotContains(t, err.Error(), "unknown field",
					"no field is misspelled here; blaming one sends the reader after a bug that does not exist")
			})
		}
	}
}

// The guard against fixing the message by loosening the decoder: a real typo in
// a single-object hand-authored body must still be named, field and all. That is
// the #1264/#1273 contract — a typo'd repo_id silently widens a one-repo Snapshot
// into an all-repo one — and it must survive this change untouched.
func TestDecodeHTTPRequest_TypoStillNamesTheUnknownField(t *testing.T) {
	var req SnapshotRequest
	err := decodeRequestBody(t, `{"repo_idd":"typo"}`, "", &req)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown field "repo_idd"`,
		"strict decoding still has to name a misspelled field for the population that wants it")
}

// And trailing bytes that are not a JSON value at all keep reporting what the
// parser actually found. "multiple JSON values" would be a second wrong answer,
// just a friendlier-sounding one.
func TestDecodeHTTPRequest_TrailingGarbageReportsTheParseError(t *testing.T) {
	var req SnapshotRequest
	err := decodeRequestBody(t, `{"repo_id":"a"} not-json`, "", &req)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed JSON request body")
	assert.Contains(t, err.Error(), "invalid character",
		"trailing bytes that do not parse are a syntax error, and saying so is what lets the reader find them")
	assert.NotContains(t, err.Error(), "multiple JSON values",
		"there is no second VALUE here — claiming one would be a new wrong diagnosis")
}

// A single well-formed body still decodes, on both populations. The trailing
// probe reads the same decoder, so getting it wrong could plausibly have broken
// the ordinary path; this is the cheap proof that it did not.
func TestDecodeHTTPRequest_SingleValueStillDecodes(t *testing.T) {
	for _, clientVersion := range []string{"", "9.9.9"} {
		var req SnapshotRequest
		require.NoError(t, decodeRequestBody(t, `{"repo_id":"repo-1","live":true}`, clientVersion, &req))
		assert.Equal(t, "repo-1", req.RepoID)
		assert.True(t, req.Live)
	}
}
