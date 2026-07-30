package session

import (
	"context"
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
