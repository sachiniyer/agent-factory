package apiclient

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

// The daemon's Host guard answers 421 with a plain-text body whose first line is
// the command that allows the name. A CLI that meets it must show that line, not
// only "malformed response envelope" — and must still treat a mutation's outcome
// as uncertain, since a non-envelope answer proves nothing about who sent it.
func TestNonEnvelopeRefusalQuotesItsBody(t *testing.T) {
	body := `af refused Host "box.ts.net:8443": not a name of this daemon. To allow it, run on the af machine:` + "\n" +
		"  af config set network.allowed_hosts box.ts.net\n"
	c := statusServer(t, func(*http.Request) (int, []byte) {
		return http.StatusMisdirectedRequest, []byte(body)
	})
	_, err := c.SetConfigValue(daemon.SetConfigValueRequest{Key: "default_program", Value: "codex"})
	if err == nil {
		t.Fatal("a 421 must be an error")
	}
	for _, want := range []string{"malformed response envelope", "HTTP 421", "af config set network.allowed_hosts box.ts.net"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err, want)
		}
	}
	if !IsMutationOutcomeUncertain(err) {
		t.Errorf("a non-envelope answer must leave a mutation's outcome uncertain: %v", err)
	}
}
