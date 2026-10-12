package apiclient

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/task"
)

// TestNon200BenignEnvelopeIsUnconfirmed is the regression guard for the
// misclassification bug in roundTrip. An intermediary in front of a REMOTE
// daemon that substitutes a benign {data,error} envelope — env.Error == nil — on
// a non-200 HTTP status must surface as an UnconfirmedHTTPResponseError: the
// handler's outcome is unknown, never a success.
//
// The daemon pairs every non-200 status with a populated env.Error
// (daemon/httpserver.go writeHTTPError), so a benign envelope on a non-200 can
// only have come from an intermediary. Before the fix, roundTrip only
// special-cased HTTP 404; every other non-200 carrying `{"data":null,"error":null}`
// unmarshaled JSON null into resp (a no-op), committedFromResponse returned nil,
// and the call was reported as success with a zero-valued response struct.
//
// It covers both carrier mechanisms by which committedFromResponse returned nil:
//   - CreateSession / AddTask: the response embeds daemon.MutationOutcome by
//     value, so a zero-valued outcome made CommittedOutcome() return (false,"")
//     and the carrier yielded nil (mechanism A — the one the headline verbs hit).
//   - SetConfigValue: the response does NOT embed MutationOutcome, so the
//     interface assertion failed and committedFromResponse returned nil at once
//     (mechanism B).
//
// Each verb is exercised across the daemon's full non-via-404 status vocabulary
// an intermediary can substitute: 500/502/503/504/400/405/413.
func TestNon200BenignEnvelopeIsUnconfirmed(t *testing.T) {
	for _, status := range non200Statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Run("CreateSession embeds MutationOutcome (mechanism A)", func(t *testing.T) {
				c := remoteStatusServer(t, func(*http.Request) (int, []byte) {
					return status, benignEnvelope
				})
				inst, err := c.CreateSession(daemon.CreateSessionRequest{Title: "alpha", Program: "claude"})
				if err == nil && inst != nil {
					t.Errorf("CreateSession projected zero-valued data into a non-nil instance (%+v); the caller would backfill a fresh ID and tag it live-ready", inst)
				}
				assertUnconfirmed(t, status, err)
			})
			t.Run("AddTask embeds MutationOutcome (mechanism A)", func(t *testing.T) {
				c := remoteStatusServer(t, func(*http.Request) (int, []byte) {
					return status, benignEnvelope
				})
				assertUnconfirmed(t, status, c.AddTask(task.Task{ID: "t1", Name: "n"}, task.ActorTUI))
			})
			t.Run("SetConfigValue does not embed MutationOutcome (mechanism B)", func(t *testing.T) {
				c := remoteStatusServer(t, func(*http.Request) (int, []byte) {
					return status, benignEnvelope
				})
				_, err := c.SetConfigValue(daemon.SetConfigValueRequest{Key: "default_program", Value: "codex"})
				assertUnconfirmed(t, status, err)
			})
		})
	}
}

// TestNon200BenignEnvelopeShapesAreUnconfirmed proves the guard catches the full
// benign surface — every JSON shape whose error member is absent/null on a
// non-200 — not just the headline `{"data":null,"error":null}`. Each parses as
// the daemon's envelope with env.Error == nil, so before the fix each could
// reach the success path (the `{"data":null}` shape decodes data to JSON null,
// a no-op into a struct, identical to the headline body).
func TestNon200BenignEnvelopeShapesAreUnconfirmed(t *testing.T) {
	shapes := []struct {
		name string
		body []byte
	}{
		{"data null error null", []byte(`{"data":null,"error":null}`)},
		{"data null error absent", []byte(`{"data":null}`)},
		{"data absent error null", []byte(`{"error":null}`)},
		{"both absent", []byte(`{}`)},
	}
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			c := remoteStatusServer(t, func(*http.Request) (int, []byte) {
				return http.StatusBadGateway, s.body
			})
			assertUnconfirmed(t, http.StatusBadGateway, c.AddTask(task.Task{ID: "t1", Name: "n"}, task.ActorTUI))
		})
	}
}

// TestNon200PopulatedEnvelopeSurfacesDaemonMessage guards the ordering: the new
// benign-envelope guard sits AFTER the env.Error branch, so a non-200 carrying
// the daemon's populated failure envelope still surfaces the daemon's verbatim
// message (through interpretEnvelopeError) rather than being reclassified as
// unconfirmed. This is the contract for a local or remote daemon's real error
// answer — the throwable statuses the rpcHandler emits (400/405/413/500/503)
// all flow through writeHTTPError with a populated env.Error.
func TestNon200PopulatedEnvelopeSurfacesDaemonMessage(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusMethodNotAllowed,
		http.StatusRequestEntityTooLarge,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := remoteStatusServer(t, func(*http.Request) (int, []byte) {
				return status, mustEnvelope(t, apiproto.Failure("daemon rejected this input"))
			})
			err := c.AddTask(task.Task{ID: "t1", Name: "n"}, task.ActorTUI)
			if err == nil {
				t.Fatalf("a %d daemon failure must surface as an error", status)
			}
			var unconfirmed *UnconfirmedHTTPResponseError
			if errors.As(err, &unconfirmed) {
				t.Fatalf("a %d carrying the daemon's populated error must surface the daemon message, not be reclassified unconfirmed, got %v", status, err)
			}
			if !strings.Contains(err.Error(), "daemon rejected this input") {
				t.Errorf("the daemon's verbatim message must survive, got: %v", err)
			}
			if IsMutationOutcomeUncertain(err) {
				t.Errorf("a daemon envelope refusal is definitive, not an uncertain outcome, got %v", err)
			}
			if IsTransportError(err) {
				t.Errorf("a daemon HTTP error answer is not a transport error, got %v", err)
			}
		})
	}
}

// TestNon200BenignEnvelopeIsUnconfirmedOnLocalSocket pins that the guard is
// transport-agnostic defense-in-depth, not gated to remote. The daemon never
// produces a benign envelope on a non-200 (it pairs every non-200 with a
// populated env.Error), so the guard is a no-op for real daemon answers on the
// local unix socket; it exists so a zero-shaped response can never become a
// successful call regardless of transport — the contract the in-code comment
// promises. Gating it to remote-only would reopen the bug the moment a local
// intermediary is ever introduced.
func TestNon200BenignEnvelopeIsUnconfirmedOnLocalSocket(t *testing.T) {
	c := statusServer(t, func(*http.Request) (int, []byte) {
		return http.StatusBadGateway, benignEnvelope
	})
	assertUnconfirmed(t, http.StatusBadGateway, c.AddTask(task.Task{ID: "t1", Name: "n"}, task.ActorTUI))
}

// assertUnconfirmed checks the invariants a non-200 benign envelope must
// satisfy after the fix: it is an UnconfirmedHTTPResponseError carrying the
// intermediary's status and the request route, classified as an uncertain
// outcome (so the mutation is neither re-sent nor reported as refused), and
// not as a transport error or a missing route.
func assertUnconfirmed(t *testing.T, status int, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("a %d carrying a benign envelope must not be reported as success", status)
	}
	var unconfirmed *UnconfirmedHTTPResponseError
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("a %d benign envelope must be an *UnconfirmedHTTPResponseError, got %T: %v", status, err, err)
	}
	if unconfirmed.Status != status {
		t.Errorf("Status = %d, want the intermediary's %d", unconfirmed.Status, status)
	}
	if !strings.HasPrefix(unconfirmed.Route, "/v1/") {
		t.Errorf("Route = %q, want the /v1/ request path", unconfirmed.Route)
	}
	if unconfirmed.Detail == "" {
		t.Errorf("Detail must quote the response body so the operator can see who answered")
	}
	if !IsMutationOutcomeUncertain(err) {
		t.Errorf("an unconfirmed non-200 must be an uncertain outcome so the mutation is not re-sent, got %v", err)
	}
	if IsTransportError(err) {
		t.Errorf("an intermediary HTTP answer is not a transport error, got %v", err)
	}
	if IsRouteNotServed(err) {
		t.Errorf("a non-404 non-200 is not a missing route, got %v", err)
	}
}

// benignEnvelope is the body of the bug: a JSON object whose data member is JSON
// null and whose error member is null, so it parses as the daemon's {data,error}
// envelope with env.Error == nil. The daemon pairs every non-200 with a
// populated env.Error, so this body on a non-200 can only have come from an
// intermediary. Before the fix, this reported success with a zero-valued
// response.
var benignEnvelope = []byte(`{"data":null,"error":null}`)

// non200Statuses is the daemon's non-200 vocabulary beyond the already-guarded
// 404: an intermediary can substitute any of these for the daemon's answer.
var non200Statuses = []int{
	http.StatusInternalServerError,   // 500
	http.StatusBadGateway,            // 502
	http.StatusServiceUnavailable,    // 503
	http.StatusGatewayTimeout,        // 504
	http.StatusBadRequest,            // 400
	http.StatusMethodNotAllowed,      // 405
	http.StatusRequestEntityTooLarge, // 413
}
