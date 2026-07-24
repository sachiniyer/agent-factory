package session

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// TestRemoteAgentDialStream_ErrorCarriesNoToken is a credential-leak regression.
//
// dialStream puts the sandbox bearer token in the URL as ?access_token= (the
// browser-WS fallback the agent-server also honours). coder/websocket's Dial
// failure is a *url.Error carrying that whole URL, so wrapping it verbatim put
// the token in the error text.
//
// That was survivable while the only failed-dial path was subscribe(), whose
// error goes to an HTTP response. #2450 made the daemon retry dials on a timer
// and LOG each failure, so on a remote session whose sandbox is down the token
// would be written to agent-factory.log in cleartext, once per backoff, for as
// long as a tab stays open. Persistent, on disk, and repeated.
//
// Note url.Redacted() is NOT a fix here: it redacts userinfo (user:pass@) only
// and leaves query parameters untouched, so the access_token would survive it.
// The value has to be removed from the query explicitly.
func TestRemoteAgentDialStream_ErrorCarriesNoToken(t *testing.T) {
	const token = "super-secret-sandbox-token-9f3a"

	// A closed port: Dial fails fast, in the connect phase, which is exactly the
	// shape of a down sandbox.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	rc, err := newRemoteAgentClient(AgentServerEndpoint{
		URL:   "http://" + addr,
		Token: token,
	}, "probe")
	if err != nil {
		t.Fatalf("newRemoteAgentClient: %v", err)
	}

	_, derr := rc.dialStream(context.Background(), 0)
	if derr == nil {
		t.Fatal("dial against a closed port returned nil error; this test needs a failed dial")
	}

	if strings.Contains(derr.Error(), token) {
		t.Fatalf("the dial error contains the sandbox bearer token in cleartext.\n\n"+
			"error: %s\n\n"+
			"#2450 logs this error on every recovery backoff, so the token would be written to "+
			"the persistent daemon log repeatedly. Strip the access_token query value before "+
			"building the error — url.Redacted() does not do it, it only covers userinfo.",
			derr.Error())
	}

	// The redaction must not gut the diagnostic: the host still has to be there,
	// or a real outage becomes unreadable in the log.
	if !strings.Contains(derr.Error(), addr) {
		t.Fatalf("the dial error no longer names the endpoint it failed to reach: %s\n\n"+
			"redaction must remove the credential, not the diagnosis", derr.Error())
	}
}

// TestRedactAccessTokenInURL pins the redaction itself, including the two ways
// it could quietly stop working: leaving the value in place, or destroying the
// rest of the URL along with it.
func TestRedactAccessTokenInURL(t *testing.T) {
	for _, tc := range []struct {
		name        string
		raw         string
		wantAbsent  string
		wantPresent []string
	}{
		{
			name:        "token value replaced, everything else kept",
			raw:         "http://box:8080/v1/sessions/s/stream?tab=2&access_token=sekrit",
			wantAbsent:  "sekrit",
			wantPresent: []string{"box:8080", "/v1/sessions/s/stream", "tab=2", accessTokenRedaction},
		},
		{
			name:        "no token, untouched",
			raw:         "http://box:8080/v1/sessions/s/stream?tab=2",
			wantAbsent:  accessTokenRedaction,
			wantPresent: []string{"box:8080", "tab=2"},
		},
		{
			name: "unparseable url says nothing rather than risk echoing it",
			// A control character makes url.Parse fail.
			raw:         "http://box:8080/\x7f?access_token=sekrit",
			wantAbsent:  "sekrit",
			wantPresent: []string{"redacted"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactAccessTokenInURL(tc.raw)
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Errorf("redactAccessTokenInURL(%q) = %q, must not contain %q", tc.raw, got, tc.wantAbsent)
			}
			for _, want := range tc.wantPresent {
				if !strings.Contains(got, want) {
					t.Errorf("redactAccessTokenInURL(%q) = %q, want it to contain %q", tc.raw, got, want)
				}
			}
		})
	}
}

// TestRedactDialErrorBackstop covers the defence that does not depend on the
// error being a *url.Error: if some other failure shape ever carries the
// credential, the structure is dropped rather than the secret.
func TestRedactDialErrorBackstop(t *testing.T) {
	const token = "tok-abc123"

	plain := errors.New("some transport failure carrying access_token=" + token + " inline")
	got := redactDialError(plain, token)
	if strings.Contains(got.Error(), token) {
		t.Fatalf("redactDialError left the token in a non-url.Error: %s", got.Error())
	}
	if !strings.Contains(got.Error(), accessTokenRedaction) {
		t.Fatalf("redactDialError scrubbed the token but left no marker: %s", got.Error())
	}

	// An error with no credential in it must survive untouched, identity included,
	// so ordinary failures keep their type for errors.Is/As.
	clean := errors.New("connection refused")
	if got := redactDialError(clean, token); got != clean {
		t.Fatalf("redactDialError replaced a clean error (%v); it must pass it through unchanged", got)
	}
	if redactDialError(nil, token) != nil {
		t.Fatal("redactDialError(nil) must stay nil")
	}
}
