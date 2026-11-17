package apiclient

import (
	"errors"
	"net/http"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// TestRestartRefusalIsReconstitutedByItsCode: an HTTP answer carrying the
// daemon's restart refusal code comes back as the typed refusal a client
// retries into the successor, whatever its wording (#7474). Without the
// daemon's own provenance, or behind a gateway status, it is unconfirmed.
func TestRestartRefusalIsReconstitutedByItsCode(t *testing.T) {
	err := interpretEnvelopeError("/v1/tasks/trigger", http.StatusServiceUnavailable, &apiproto.EnvelopeError{
		Message: "the daemon is cycling", Code: apiproto.ErrorCodeDaemonRestarting, DaemonRejected: true,
	})
	if !apiproto.IsLifecycleRefusal(err, apiproto.ErrorCodeDaemonRestarting) {
		t.Fatalf("got %T %v, want the typed restart refusal", err, err)
	}
	if err.Error() != "the daemon is cycling" {
		t.Fatalf("message %q, want the daemon's own", err.Error())
	}

	for _, tc := range []struct {
		name   string
		status int
		env    apiproto.EnvelopeError
	}{
		{"no daemon provenance", http.StatusServiceUnavailable, apiproto.EnvelopeError{Code: apiproto.ErrorCodeDaemonRestarting}},
		{"a gateway status", http.StatusBadGateway, apiproto.EnvelopeError{Code: apiproto.ErrorCodeDaemonRestarting, DaemonRejected: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := interpretEnvelopeError("/v1/tasks/trigger", tc.status, &tc.env)
			var unconfirmed *UnconfirmedHTTPResponseError
			if !errors.As(err, &unconfirmed) {
				t.Fatalf("got %T %v, want unconfirmed", err, err)
			}
		})
	}
}
