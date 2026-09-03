package apiclient

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// DaemonVersionPhrase's three answers (#3679, #3708). It renders the clause a
// skew refusal drops into "that daemon (%s) does not serve the %s route", and
// two callers now depend on it — `af config set --daemon-url` and the TUI config
// editor — so each branch is pinned here rather than incidentally through
// whichever refusal happens to be tested.
//
// The point of the three answers is that NONE of them is "unknown": each states
// a different fact about the daemon, and a caller reading the refusal can act on
// which one it got.
func TestDaemonVersionPhraseStatesWhatTheDaemonSaid(t *testing.T) {
	t.Run("a daemon that reports a version", func(t *testing.T) {
		c := statusServer(t, func(*http.Request) (int, []byte) {
			return http.StatusOK, mustEnvelope(t, apiproto.Success(daemon.PingResponse{OK: true, Version: "0.9.1"}))
		})
		if got := c.DaemonVersionPhrase(context.Background()); got != "version 0.9.1" {
			t.Errorf("phrase = %q, want %q", got, "version 0.9.1")
		}
	})

	// An empty Version from a RESPONDING daemon is positive evidence, not a
	// missing answer: the field has ridden Ping since #1044, so a daemon that
	// omits it predates that — which is consistent with it also predating the
	// route the caller could not reach.
	t.Run("a daemon too old to report one", func(t *testing.T) {
		c := statusServer(t, func(*http.Request) (int, []byte) {
			return http.StatusOK, mustEnvelope(t, apiproto.Success(daemon.PingResponse{OK: true}))
		})
		got := c.DaemonVersionPhrase(context.Background())
		if !strings.Contains(got, "predates version reporting") {
			t.Errorf("phrase = %q, want it to say the daemon predates version reporting", got)
		}
		if strings.Contains(got, "unknown") {
			t.Errorf("an omitted version is evidence, not an unknown: %q", got)
		}
	})

	// A probe that cannot complete is a third fact again, and it carries the
	// transport error so the reader learns WHY — a refused socket reads
	// differently from a 401.
	t.Run("a daemon that cannot be reached", func(t *testing.T) {
		c := NewWithSocket(testguard.SocketPath(t, "dead.sock"))
		got := c.DaemonVersionPhrase(context.Background())
		if !strings.HasPrefix(got, "its version could not be read:") {
			t.Errorf("phrase = %q, want it to report the failed probe as its own fact", got)
		}
		if strings.Contains(got, "version 0") || strings.Contains(got, "predates") {
			t.Errorf("an unreachable daemon must not be described as a version: %q", got)
		}
	})
}
