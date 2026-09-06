package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"

	"github.com/stretchr/testify/require"
)

func TestHTTPErrorDaemonProvenance(t *testing.T) {
	for _, status := range []int{400, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeHTTPError(rec, httptest.NewRequest(http.MethodPost, "/v1/CreateSession", nil), status, errors.New("refused"))
			var env struct {
				Error map[string]any `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			require.Equal(t, true, env.Error["daemon_rejected"])
			require.NotContains(t, env.Error, "code")
		})
	}
}

// Walk the served table so newly added RPCs inherit the same provenance contract.
// Malformed input stops before dispatch and never invokes session lifecycle work.
func TestHTTPRoutesDaemonProvenance(t *testing.T) {
	for _, route := range servedHTTPRoutes() {
		if route.Method != http.MethodPost {
			continue
		}
		t.Run(route.Path, func(t *testing.T) {
			rec := doHTTP(&controlServer{}, http.MethodPost, route.Path, `{`)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			var env struct {
				Error map[string]any `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			require.Equal(t, true, env.Error["daemon_rejected"])
		})
	}
}

func TestHTTPCommittedErrorNotRejected(t *testing.T) {
	rec := httptest.NewRecorder()
	writeHTTPError(rec, httptest.NewRequest(http.MethodPost, "/v1/UpdateTask", nil), 500,
		&mutationCommittedError{err: errors.New("committed follow-up failed")})
	var env struct {
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, "mutation_committed", env.Error["code"])
	require.NotContains(t, env.Error, "daemon_rejected")
}

func TestHTTPEnvelopeOrdinaryCodedErrorProvenance(t *testing.T) {
	rec := httptest.NewRecorder()
	env := apiproto.FailureWithCode("refused", "admission_refused")
	writeHTTPEnvelope(rec, httptest.NewRequest(http.MethodPost, "/v1/CreateSession", nil), 503, env)
	var decoded struct {
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &decoded))
	require.Equal(t, true, decoded.Error["daemon_rejected"])
	require.Equal(t, "admission_refused", decoded.Error["code"])
	require.False(t, env.Error.DaemonRejected, "writer must not mutate the caller's envelope")
}
