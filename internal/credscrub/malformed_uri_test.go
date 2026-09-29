package credscrub

import (
	"strings"
	"testing"
)

// TestScrubRedactsMalformedPercentURIComponents guards against a leak where a
// credential placed in a URI query pair or fragment, partially
// percent-encoded (so the outer credential shape regex no longer matches
// the raw bytes) plus one malformed percent escape (e.g. %2g, %ZZ),
// survived Scrub verbatim. The malformed escape makes the URI transform
// fail-close the undecodable component, so neither the credential-shape
// producer nor the engine's fail-closed path lets the bytes ship: the whole
// component is redacted, malformed tail included.
//
// This inverts the keep-view predecessor (malformed_uri_test.go on the
// superseded #4660): the adversarial inputs — a partially-encoded AWS
// access key id and GitHub PAT across query and fragment, the %ZZ spelling
// variant, and well-formed controls — are retained; the malformed-case
// assertions flip from "the secret is redacted but the malformed tail ships
// as ordinary data" to "the whole component fails closed, so the malformed
// tail does not ship either."
func TestScrubRedactsMalformedPercentURIComponents(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// exactWant is asserted for the well-formed controls, whose output
		// the producer redaction determines and fail-close does not touch.
		exactWant string
		// recoverableRaw is the still-percent-encoded surface that must
		// NOT survive in any case.
		recoverableRaw string
		// plaintext is the fully decoded secret that must NOT survive in
		// any case.
		plaintext string
		// malformedTail, when non-empty, marks a malformed input. The
		// keep-view design left this tail in the output; under fail-close
		// the whole component is redacted, so the tail must NOT survive
		// and the fail-close marker must be present.
		malformedTail string
	}{
		// AWS access key id, one byte (%49 -> 'I') escaped to break the
		// AKIA[0-9A-Z]{16} shape, plus one malformed escape.
		{
			name:           "query-malformed-akia",
			in:             "fetch http://h/?AKIA%49OSFODNN7EXAMPLE%2g end",
			recoverableRaw: "AKIA%49OSFODNN7EXAMPLE",
			plaintext:      "AKIAIOSFODNN7EXAMPLE",
			malformedTail:  "%2g",
		},
		{
			name:           "fragment-malformed-akia",
			in:             "http://h/p#AKIA%49OSFODNN7EXAMPLE%2g",
			recoverableRaw: "AKIA%49OSFODNN7EXAMPLE",
			plaintext:      "AKIAIOSFODNN7EXAMPLE",
			malformedTail:  "%2g",
		},
		// Well-formed controls (no malformed escape): the view path already
		// redacted these. Asserted byte-for-byte to prove no regression, and
		// to pin that fail-close did not widen to well-formed inputs.
		{
			name:           "query-wellformed-akia",
			in:             "fetch http://h/?AKIA%49OSFODNN7EXAMPLE end",
			exactWant:      "fetch http://h/?[redacted-secret] end",
			recoverableRaw: "AKIA%49OSFODNN7EXAMPLE",
			plaintext:      "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:           "fragment-wellformed-akia",
			in:             "http://h/p#AKIA%49OSFODNN7EXAMPLE",
			exactWant:      "http://h/p#[redacted-secret]",
			recoverableRaw: "AKIA%49OSFODNN7EXAMPLE",
			plaintext:      "AKIAIOSFODNN7EXAMPLE",
		},
		// Other malformed spellings confirm the fail-close is not %2g-specific.
		{
			name:           "query-malformed-akia-percent-ZZ",
			in:             "fetch http://h/?AKIA%49OSFODNN7EXAMPLE%ZZ end",
			recoverableRaw: "AKIA%49OSFODNN7EXAMPLE",
			plaintext:      "AKIAIOSFODNN7EXAMPLE",
			malformedTail:  "%ZZ",
		},
		// GitHub PAT, one byte (%30 -> '0') escaped plus malformed escape:
		// proves the leak/fix is not specific to the AWS access key id shape.
		{
			name:           "query-malformed-github-pat",
			in:             "http://h/?k=ghp_%30123456789abcdefghij%2g",
			recoverableRaw: "ghp_%30123456789abcdefghij",
			plaintext:      "ghp_0123456789abcdefghij",
			malformedTail:  "%2g",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Scrub(c.in)
			t.Logf("in : %q", c.in)
			t.Logf("out: %q", got)
			if c.exactWant != "" && got != c.exactWant {
				t.Errorf("exact mismatch: got %q, want %q", got, c.exactWant)
			}
			if strings.Contains(got, c.recoverableRaw) {
				t.Errorf("LEAK recoverable raw-encoded secret survived: %q", got)
			}
			if strings.Contains(got, c.plaintext) {
				t.Errorf("LEAK plaintext secret survived: %q", got)
			}
			if c.malformedTail != "" {
				// Fail-close: the whole malformed component is redacted, so
				// the trailing malformed escape — which the keep-view
				// predecessor left intact — must NOT survive, and the
				// fail-close marker must be present.
				if strings.Contains(got, c.malformedTail) {
					t.Errorf("malformed tail %q survived (not fail-closed): %q", c.malformedTail, got)
				}
				if !strings.Contains(got, RedactedMarker) {
					t.Errorf("malformed component not fail-closed (no %q): %q", RedactedMarker, got)
				}
			} else {
				// Well-formed control: the producer matched the decoded view
				// and redacted the secret to SecretMarker.
				if !strings.Contains(got, SecretMarker) {
					t.Errorf("well-formed secret was not redacted (no %q): %q", SecretMarker, got)
				}
			}
		})
	}
}
