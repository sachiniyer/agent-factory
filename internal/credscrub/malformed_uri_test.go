package credscrub

import (
	"strings"
	"testing"
)

// TestScrubRedactsMalformedPercentURIComponents guards against a leak where a
// credential placed in a URI query pair or fragment, partially
// percent-encoded (so the outer credential shape regex no longer matches
// the raw bytes) plus one malformed percent escape (e.g. %2g, %ZZ),
// survived Scrub verbatim. The malformed escape made the URI transform
// drop PercentDecode's stable view, so neither the credential-shape
// producer nor the engine's fail-closed path ever saw the bytes. The
// recoverable, still-encoded bytes (e.g. AKIA%49OSFODNN7EXAMPLE, where
// %49 decodes to 'I') then rode through to the output; anyone
// URL-decoding the output recovered the secret intact.
func TestScrubRedactsMalformedPercentURIComponents(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// exactWant is asserted when present; otherwise the leak/marker
		// checks below are the contract.
		exactWant      string
		recoverableRaw string // the still-percent-encoded surface that must NOT survive
		plaintext      string // the fully decoded secret that must NOT survive
	}{
		// AWS access key id, one byte (%49 -> 'I') escaped to break the
		// AKIA[0-9A-Z]{16} shape, plus one malformed escape.
		{
			name:           "query-malformed-akia",
			in:             "fetch http://h/?AKIA%49OSFODNN7EXAMPLE%2g end",
			exactWant:      "fetch http://h/?[redacted-secret]%2g end",
			recoverableRaw: "AKIA%49OSFODNN7EXAMPLE",
			plaintext:      "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:           "fragment-malformed-akia",
			in:             "http://h/p#AKIA%49OSFODNN7EXAMPLE%2g",
			exactWant:      "http://h/p#[redacted-secret]%2g",
			recoverableRaw: "AKIA%49OSFODNN7EXAMPLE",
			plaintext:      "AKIAIOSFODNN7EXAMPLE",
		},
		// Well-formed controls (no malformed escape): the view path already
		// redacted these. Asserted byte-for-byte to prove no regression.
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
		// Other malformed spellings confirm the fix is not %2g-specific.
		{
			name:           "query-malformed-akia-percent-ZZ",
			in:             "fetch http://h/?AKIA%49OSFODNN7EXAMPLE%ZZ end",
			exactWant:      "fetch http://h/?[redacted-secret]%ZZ end",
			recoverableRaw: "AKIA%49OSFODNN7EXAMPLE",
			plaintext:      "AKIAIOSFODNN7EXAMPLE",
		},
		// GitHub PAT, one byte (%30 -> '0') escaped plus malformed escape:
		// proves the leak is not specific to the AWS access key id shape.
		{
			name:           "query-malformed-github-pat",
			in:             "http://h/?k=ghp_%30123456789abcdefghij%2g",
			recoverableRaw: "ghp_%30123456789abcdefghij",
			plaintext:      "ghp_0123456789abcdefghij",
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
			if !strings.Contains(got, SecretMarker) {
				t.Errorf("secret was not redacted (no %q marker): %q", SecretMarker, got)
			}
		})
	}
}
