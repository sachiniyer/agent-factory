package apiclient

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// A committed add whose follow-up failed answers an error with no body, so the
// stored id rides the envelope's error member: the caller of a daemon that mints
// the id (a sandbox credential's add) can still get and remove the task (#5890).
func TestCommittedEnvelopeCarriesTheStoredID(t *testing.T) {
	err := interpretEnvelopeError("AddTask", http.StatusInternalServerError, &apiproto.EnvelopeError{
		Message: "task saved but the scheduler reload failed", Code: apiproto.ErrorCodeMutationCommitted, StoredID: "ab12cd34",
	})
	require.True(t, IsMutationCommitted(err))
	assert.Equal(t, "ab12cd34", apiproto.StoredID(err))
	assert.Equal(t, "ab12cd34", apiproto.StoredID(fmt.Errorf("wrapped: %w", err)), "matches through wrappers")

	// A refusal or an unconfirmed (gateway) answer names no stored record.
	assert.Empty(t, apiproto.StoredID(errors.New("plain")))
	gw := interpretEnvelopeError("AddTask", http.StatusBadGateway, &apiproto.EnvelopeError{
		Message: "x", Code: apiproto.ErrorCodeMutationCommitted, StoredID: "ab12cd34",
	})
	assert.Empty(t, apiproto.StoredID(gw), "a proxy's answer proves nothing about what was stored")
}
