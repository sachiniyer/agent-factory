package apiclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"
)

// outcomeServer binds a real Unix-socket HTTP server whose /v1/AddTask handler
// is h, counts the requests that reach it, and returns a Client dialing it.
func outcomeServer(t *testing.T, h http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/AddTask", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewWithSocket(sockPath), &hits
}

// hangUp drops the connection without answering, after the handler ran — the
// shape of a daemon that committed and then exited, or a reset mid-reply.
func hangUp(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_ = conn.Close()
}

func addTask(c *Client) error {
	return c.AddTask(task.Task{ID: "t1", Name: "n"}, task.ActorTUI)
}

// TestTransportError_NeverSentCases pins the failures a mutation may re-send:
// no connection was ever obtained, so no request byte left the process.
func TestTransportError_NeverSentCases(t *testing.T) {
	t.Run("missing socket (daemon-HTTP bind race)", func(t *testing.T) {
		c := NewWithSocket(testguard.SocketPath(t, "absent.sock"))
		err := addTask(c)
		if !IsTransportError(err) || !IsTransportErrorNotSent(err) {
			t.Fatalf("missing socket must be a never-sent TransportError, got %T %v", err, err)
		}
		if IsMutationOutcomeUncertain(err) {
			t.Fatal("a request that never left the process cannot have committed")
		}
	})
	t.Run("refused dial (socket file, no listener)", func(t *testing.T) {
		sockPath := testguard.SocketPath(t, "stale.sock")
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatalf("listen unix: %v", err)
		}
		// Keep the file but stop accepting: the kernel refuses the dial.
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = ln.Close()
		if _, statErr := os.Stat(sockPath); statErr != nil {
			t.Fatalf("precondition: stale socket file must remain: %v", statErr)
		}
		err = addTask(NewWithSocket(sockPath))
		if !IsTransportErrorNotSent(err) {
			t.Fatalf("a refused dial must be a never-sent TransportError, got %T %v", err, err)
		}
		if IsMutationOutcomeUncertain(err) {
			t.Fatal("a refused dial cannot have committed")
		}
	})
}

// TestTransportError_MaybeReceivedCases pins the failures a mutation must never
// re-send: the daemon ran the handler and only the reply was lost (#4820).
func TestTransportError_MaybeReceivedCases(t *testing.T) {
	t.Run("connection dropped before any reply", func(t *testing.T) {
		c, hits := outcomeServer(t, func(w http.ResponseWriter, _ *http.Request) { hangUp(t, w) })
		err := addTask(c)
		if hits.Load() != 1 {
			t.Fatalf("precondition: the handler must have run once, ran %d", hits.Load())
		}
		if !IsTransportError(err) || IsTransportErrorNotSent(err) {
			t.Fatalf("a dropped reply must be a maybe-received TransportError, got %T %v", err, err)
		}
		if !IsMutationOutcomeUncertain(err) {
			t.Fatal("the handler ran: the outcome is uncertain, not refused")
		}
	})
	t.Run("connection dropped mid-body", func(t *testing.T) {
		c, hits := outcomeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":`))
			http.NewResponseController(w).Flush() //nolint:errcheck // best effort
			hangUp(t, w)
		})
		err := addTask(c)
		if hits.Load() != 1 {
			t.Fatalf("precondition: the handler must have run once, ran %d", hits.Load())
		}
		if !IsTransportError(err) || IsTransportErrorNotSent(err) {
			t.Fatalf("a truncated reply must be a maybe-received TransportError, got %T %v", err, err)
		}
		if !IsMutationOutcomeUncertain(err) {
			t.Fatal("a truncated reply is an uncertain outcome")
		}
	})
	t.Run("malformed envelope", func(t *testing.T) {
		c, _ := outcomeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
		})
		err := addTask(c)
		if IsTransportError(err) {
			t.Fatalf("a malformed reply is not a transport error (reads must not start retrying it), got %v", err)
		}
		if !IsMutationOutcomeUncertain(err) {
			t.Fatalf("an undecodable reply proves nothing about the handler: %v", err)
		}
	})
}

// TestIsMutationOutcomeUncertain_DaemonAnswers covers the daemon's own answers:
// an envelope refusal and an admission refusal are definitive, a committed
// outcome is not a refusal.
func TestIsMutationOutcomeUncertain_DaemonAnswers(t *testing.T) {
	envelope := func(env apiproto.Envelope) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = apiproto.WriteEnvelope(w, env)
		}
	}
	t.Run("envelope refusal", func(t *testing.T) {
		c, _ := outcomeServer(t, envelope(apiproto.Envelope{Error: &apiproto.EnvelopeError{Message: "invalid cron"}}))
		if err := addTask(c); err == nil || IsMutationOutcomeUncertain(err) || IsTransportError(err) {
			t.Fatalf("a daemon refusal is definitive, got %v", err)
		}
	})
	t.Run("admission refusal", func(t *testing.T) {
		msg := "agent-factory daemon is starting (restoring sessions); retry shortly"
		c, _ := outcomeServer(t, envelope(apiproto.Envelope{Error: &apiproto.EnvelopeError{Message: msg}}))
		err := addTask(c)
		if !daemon.IsDaemonAdmissionRetryable(err) {
			t.Fatalf("precondition: admission classifier must match, got %v", err)
		}
		if IsMutationOutcomeUncertain(err) {
			t.Fatal("an admission refusal is answered before the handler runs; it is not uncertain")
		}
	})
	t.Run("committed", func(t *testing.T) {
		c, _ := outcomeServer(t, envelope(apiproto.Envelope{Error: &apiproto.EnvelopeError{
			Message: "saved; refresh failed", Code: apiproto.ErrorCodeMutationCommitted,
		}}))
		if err := addTask(c); !IsMutationCommitted(err) || !IsMutationOutcomeUncertain(err) {
			t.Fatalf("a committed outcome must never read as a refusal, got %v", err)
		}
	})
}

// TestIsMutationOutcomeUncertain_Shapes covers the classifier on hand-built
// errors, including the fail-safe zero value.
func TestIsMutationOutcomeUncertain_Shapes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("session not found"), false},
		{"zero-value TransportError fails safe", &TransportError{Err: errors.New("x")}, true},
		{"never-sent TransportError", &TransportError{Err: errors.New("x"), NotSent: true}, false},
		{"never-sent wrapping a dead context", &TransportError{
			Err: &url.Error{Op: "Post", Err: context.DeadlineExceeded}, NotSent: true}, false},
		{"bare deadline", context.DeadlineExceeded, true},
		{"unconfirmed 404", &UnconfirmedHTTPResponseError{Route: "/v1/AddTask", Status: 404}, true},
		{"not served", &RouteNotServedError{Route: "/v1/AddTask"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsMutationOutcomeUncertain(tc.err); got != tc.want {
				t.Fatalf("IsMutationOutcomeUncertain(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
