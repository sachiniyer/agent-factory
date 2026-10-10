package apiclient

import (
	"context"
	"net"
	"strings"
	"testing"
)

// extractFirstAccessTokenValue echoes the agentproto test helper: the bytes after
// the first "access_token=" up to a value terminator (whitespace, #, quote, &),
// so the assertion sees exactly what the redactor left in the credential
// position. Asserting this equals "REDACTED" — rather than
// strings.Contains(err, token) — avoids the false positive the bug report
// warns about: the correct marker "REDACTED" itself contains the token for a
// marker-substring token (e.g. token "RED"), so a Contains check would flag the
// CORRECT output as a leak.
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

// TestDialStreamErrorRedactsMarkerSubstringToken is the P2 end-to-end procedure
// from the test plan: it drives the production call site at apiclient/stream.go
// (DialStream → RedactAccessTokenError(err, c.token)) with a degenerate,
// marker-substring access_token and asserts the redaction boundary holds at the
// dialer, not just at the agentproto helper.
//
// A remote client whose token is a case-sensitive prefix of "REDACTED" (the
// shape the bug report identifies: an operator who overwrites ~/.af/daemon-token
// with a 1–7 char uppercase string) dials an unreachable daemon port. The
// underlying websocket.Dial returns a *url.Error whose URL is the dial URL with
// ?access_token=<token>; RedactAccessTokenError sees that *url.Error, the URL
// pass writes the marker, and the literal-token catch-all runs on token=<prefix>.
// Before the fix the catch-all matched inside the marker and extended it (e.g.
// access_token=REDACTEDACTED for token "RED"), re-injecting the secret into the
// TransportError the caller and the TUI log see. After the fix the value is
// exactly the 8-byte marker.
//
// The assertion is the exact-marker value, NOT strings.Contains(err, token):
// the correct marker contains the token for every marker-substring token, so
// Contains would false-positive on the fixed output. A non-substring sentinel
// is included to guard the no-regression half (the realistic 43-char token
// still redacts and the failed endpoint still survives for diagnosis).
func TestDialStreamErrorRedactsMarkerSubstringToken(t *testing.T) {
	for _, token := range []string{"R", "RE", "RED", "REDA", "REDAC", "REDACT", "REDACTE"} {
		token := token
		t.Run(token, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			addr := ln.Addr().String()
			ln.Close()

			c, err := NewRemote("http://"+addr, token)
			if err != nil {
				t.Fatalf("NewRemote: %v", err)
			}
			_, dialErr := c.DialStream(context.Background(), "probe", "", "", 0, 0)
			if dialErr == nil {
				t.Fatal("dial against a closed port returned nil error")
			}
			got := dialErr.Error()
			if v := extractFirstAccessTokenValue(got); v != "REDACTED" {
				t.Errorf("token %q: access_token value = %q, want exactly REDACTED (marker extended); err: %s",
					token, v, got)
			}
			// The failed endpoint must survive for diagnosis (the existing
			// TestDialStreamErrorDoesNotExposeAccessToken contract).
			if !strings.Contains(got, addr) {
				t.Errorf("token %q: redaction removed the failed endpoint needed for diagnosis: %s", token, got)
			}
		})
	}

	// No-regression: a realistic, non-substring token still redacts and keeps the
	// endpoint. This is the existing sentinel; reproduced here so the
	// marker-substring loop cannot mask a regression in the common case.
	const sentinel = "af-sentinel-2644-do-not-log"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	c, err := NewRemote("http://"+addr, sentinel)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	_, dialErr := c.DialStream(context.Background(), "probe", "", "", 0, 0)
	if dialErr == nil {
		t.Fatal("dial against a closed port returned nil error")
	}
	got := dialErr.Error()
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
