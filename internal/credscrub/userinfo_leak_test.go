package credscrub

import (
	"net/url"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// TestUserinfoMalformedFailClosed exercises the leak mechanism through the real
// composite log boundary (log/redact.go:49 calls
// agentproto.RedactAccessTokenText(credscrub.Scrub(s))) for a malformed percent
// escape in URI userinfo — the carrier the sibling fail-close fix in a754a7ac
// missed. The trigger is a doubly-constrained conjunction: the credential in
// userinfo is percent-encoded enough to hide its plaintext shape from the raw
// shape sweep, AND it is terminated by a malformed escape so url.Parse rejects
// the whole URI and no userinfo view is ever produced. Either condition alone
// redacts; both together leaked the recoverable percent-encoded surface before
// the fix. The argument for the fix is symmetry with the path/query/fragment
// fail-closes the maintainers already accepted for the identical input class.
//
// The token is a placeholder matching the classic GitHub PAT shape
// (ghp_[A-Za-z0-9]{20,}), the same dummy family as conformance_test.go:15; it is
// not a live credential.
func TestUserinfoMalformedFailClosed(t *testing.T) {
	// PAT-shape placeholder with one byte ('0') percent-encoded (%30) so the
	// plaintext does not appear in the raw text and the shape regex cannot
	// match across the %.
	const encodedPAT = "ghp_%30123456789abcdefghij0123456789ABCD"
	// The same token URL-decoded — the plaintext recoverable from the
	// percent-encoded surface by an adversary who strips the malformed tail
	// and unescapes the remainder.
	decodedPAT, derr := url.QueryUnescape("ghp_%30123456789abcdefghij0123456789ABCD")
	if derr != nil || decodedPAT != "ghp_0123456789abcdefghij0123456789ABCD" {
		t.Fatalf("setup: unexpected decode %q err=%v", decodedPAT, derr)
	}

	// Malformed userinfo: url.Parse fails, no userinfo view is produced.
	malformedIn := "log: http://" + encodedPAT + "%2g:pass@host/p done"

	// Well-formed control: url.Parse succeeds, the userinfo decodes and the
	// shape matcher redacts the plaintext PAT through the producer view.
	t.Run("wellformed-userinfo-redacts", func(t *testing.T) {
		out := Scrub("log: http://" + encodedPAT + ":pass@host/p done")
		if strings.Contains(out, decodedPAT) {
			t.Fatalf("well-formed userinfo shipped the plaintext PAT: %q", out)
		}
		if !strings.Contains(out, SecretMarker) {
			t.Fatalf("well-formed userinfo was not redacted: %q", out)
		}
		if !strings.HasPrefix(strings.Replace(out, "log: ", "", 1), "http://"+SecretMarker+":pass@host/p done") {
			t.Fatalf("well-formed userinfo redaction shape unexpected: %q", out)
		}
	})

	// The malformed case must fail closed, not ship the recoverable surface.
	t.Run("malformed-userinfo-failclosed", func(t *testing.T) {
		out := Scrub(malformedIn)
		if strings.Contains(out, encodedPAT) {
			t.Fatalf("malformed userinfo shipped the recoverable percent-encoded surface: %q", out)
		}
		if strings.Contains(out, decodedPAT) {
			t.Fatalf("malformed userinfo shipped the plaintext PAT: %q", out)
		}
		if !strings.Contains(out, RedactedMarker) {
			t.Fatalf("malformed userinfo not fail-closed: %q", out)
		}
	})

	// The leak mechanism through the full log boundary: RedactAccessTokenText
	// scans for access_token= substrings, not ghp_* shapes, so it must not
	// re-introduce the recoverable surface that credscrub.Scrub failed to close.
	t.Run("log-boundary-failclosed", func(t *testing.T) {
		out := agentproto.RedactAccessTokenText(Scrub(malformedIn))
		if strings.Contains(out, encodedPAT) || strings.Contains(out, decodedPAT) {
			t.Fatalf("log boundary shipped the recoverable surface: %q", out)
		}
		if !strings.Contains(out, RedactedMarker) {
			t.Fatalf("log boundary not fail-closed: %q", out)
		}
	})
}
