package session

import (
	"context"
	"net"
	"strings"
	"testing"
)

// extractFirstAccessTokenValue is the exact-marker assertion helper (mirrors the
// agentproto and apiclient ones): the bytes after the first "access_token=" up
// to a value terminator. Asserting this equals "REDACTED" — rather than
// strings.Contains(err, token) — avoids the false positive the bug report
// warns about: the correct marker "REDACTED" contains the token for a
// marker-substring token (e.g. token "RED"), so a Contains check would flag the
// CORRECT output as a leak. The existing TestRemoteAgentDialStream_ErrorCarriesNoToken
// uses the Contains form, which is correct for its realistic, non-substring sentinel
// but cannot be reused for a marker-substring token.
func extractFirstAccessTokenValue(s string) string {
	i := strings.Index(s, "access_token=")
	if i < 0 {
		return ""
	}
	s = s[i+len("access_token="):]
	if j := strings.IndexAny(s, " \t\r\n#\"'&"); j >= 0 {
		s = s[:j]
	}
	return s
}

// TestRemoteAgentDialStream_ErrorDoesNotExtendRedactionMarker is the P2
// end-to-end procedure for the second dialer call site
// (session/agentserver_remote.go:738, dialStream →
// agentproto.RedactAccessTokenError(err, c.token)). It is the marker-substring
// counterpart to TestRemoteAgentDialStream_ErrorCarriesNoToken.
//
// dialStream puts the sandbox bearer token in the dial URL as ?access_token=,
// and coder/websocket's Dial failure is a *url.Error carrying that whole URL, so
// RedactAccessTokenError sees the *url.Error, the URL pass writes the marker,
// and the literal-token catch-all runs on token=<value>. When the operator
// supplies a marker-substring token (the closed set the bug report identifies),
// the pre-fix catch-all matched inside the marker and extended it (e.g.
// access_token=REDACTEDACTED for token "RED"), re-injecting the secret into the
// error the #2450 recovery timer logs to agent-factory.log on every backoff.
// After the fix the value is exactly the 8-byte marker.
//
// The assertion is the exact-marker value, NOT strings.Contains(err, token):
// the correct marker contains the token for every marker-substring token, so
// Contains would false-positive on the fixed output. A non-substring sentinel
// is included to guard the no-regression half (the realistic token still redacts
// and the failed endpoint still survives for diagnosis).
func TestRemoteAgentDialStream_ErrorDoesNotExtendRedactionMarker(t *testing.T) {
	for _, token := range []string{"R", "RE", "RED", "REDA", "REDAC", "REDACT", "REDACTE"} {
		token := token
		t.Run(token, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			addr := ln.Addr().String()
			ln.Close()

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
			got := derr.Error()
			if v := extractFirstAccessTokenValue(got); v != "REDACTED" {
				t.Errorf("token %q: access_token value = %q, want exactly REDACTED (marker extended); err: %s",
					token, v, got)
			}
			// The failed endpoint must survive for diagnosis (mirrors the
			// existing TestRemoteAgentDialStream_ErrorCarriesNoToken contract).
			if !strings.Contains(got, addr) {
				t.Errorf("token %q: redaction removed the failed endpoint needed for diagnosis: %s", token, got)
			}
		})
	}

	// No-regression: a realistic, non-substring token still redacts and keeps
	// the endpoint. The existing test uses Contains for this sentinel (safe
	// because the sentinel is not a substring of REDACTED); reproduced here so
	// the marker-substring loop cannot mask a regression in the common case.
	const sentinel = "super-secret-sandbox-token-9f3a"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	rc, err := newRemoteAgentClient(AgentServerEndpoint{
		URL:   "http://" + addr,
		Token: sentinel,
	}, "probe")
	if err != nil {
		t.Fatalf("newRemoteAgentClient: %v", err)
	}
	_, derr := rc.dialStream(context.Background(), 0)
	if derr == nil {
		t.Fatal("dial against a closed port returned nil error")
	}
	got := derr.Error()
	if strings.Contains(got, sentinel) {
		t.Fatalf("realistic sentinel token survived redaction: %s", got)
	}
	if v := extractFirstAccessTokenValue(got); v != "REDACTED" {
		t.Fatalf("realistic token: access_token value = %q, want REDACTED: %s", v, got)
	}
	if !strings.Contains(got, addr) {
		t.Fatalf("realistic token: failed endpoint removed: %s", got)
	}
}
