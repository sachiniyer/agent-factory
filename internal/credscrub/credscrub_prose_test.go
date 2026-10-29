package credscrub

import (
	"strings"
	"testing"
)

// TestScrubKeepsProseAfterQuotedKeyLikeTitle pins #6054: a quoted title that
// merely ENDS in a key word (`"feat-redesign-pr1-tokens": `) followed by two
// ordinary words is prose, not a credential assignment. The production line was
//
//	session "feat-redesign-pr1-tokens": tracked worktree /home/… is gone
//
// and both words were replaced by the marker.
func TestScrubKeepsProseAfterQuotedKeyLikeTitle(t *testing.T) {
	for _, title := range []string{
		"feat-redesign-pr1-tokens", "p3-pr2-withauth", "sentry-fix-github-oauth",
		"fix-3978-pwa-chrome-tokens", "feat-1696-web-auth", "p3-pr1-auth", "fix-4585-longpress-token",
		"x-tokens",
	} {
		in := `session "` + title + `": tracked worktree /home/u/.agent-factory/archived/0f8f/` + title + ` is gone (deleted outside af)`
		if got := Scrub(in); got != in {
			t.Errorf("prose after quoted title %q was altered:\n in: %q\nout: %q", title, in, got)
		}
		if spans := Redactions(in); len(spans) != 0 {
			t.Errorf("prose after quoted title %q produced redaction spans: %v", title, spans)
		}
	}
}

// TestScrubStillCatchesCredentialsAfterQuotedKey is the control: the same
// quoted-key shape followed by a credential-shaped value keeps redacting, so the
// prose fix does not weaken the scrubber.
func TestScrubStillCatchesCredentialsAfterQuotedKey(t *testing.T) {
	for _, in := range []string{
		`"api_token": ` + sentinel + `abcdef`,
		`"auth": Bearer ` + sentinel + `abcdef`,
		`"auth": CustomScheme ` + sentinel + `abcdefghij`,
		`"x-tokens": ` + sentinel + `abc tail`,
		`{"password": "` + sentinel + `abcdef"}`,
		`'secret': ` + sentinel + `abcdef`,
		`"token": ` + sentinel + `abcdef extra`,
	} {
		if got := Scrub(in); strings.Contains(got, sentinel) {
			t.Errorf("credential survived after a quoted key: in %q out %q", in, got)
		}
	}
}

// TestScrubQuotedKeyMixedCaseSchemeStrandsNoToken pins the #6057 review: the
// prose check must judge the WHOLE scheme value. A mixed-case scheme before an
// all-lowercase token was judged on the token alone, read as prose, and was
// skipped by the keyed-scheme matcher while appendKeyValueSpans still redacted
// the scheme — leaving `"auth": [redacted-secret] abcdefghijklmnop`, a
// credential in the log that only a second scrub removed.
func TestScrubQuotedKeyMixedCaseSchemeStrandsNoToken(t *testing.T) {
	for _, in := range []string{
		`"auth": CustomScheme abcdefghijklmnop`,
		`'auth': CustomScheme abcdefghijklmnop`,
	} {
		got := Scrub(in)
		if strings.Contains(got, "abcdefghijklmnop") {
			t.Errorf("token stranded after a redacted scheme: in %q out %q", in, got)
		}
		if again := Scrub(got); again != got {
			t.Errorf("not idempotent: in %q once %q twice %q", in, got, again)
		}
	}
}

// TestScrubQuotedKeyMarkerPrefixedValueIsNotProse pins the #6057 review: the
// prose exemption must not bypass the exact-marker safeguard. A bare value that
// merely BEGINS with a redaction marker is a credential with a marker glued on,
// and brackets alone do not make a lowercase value credential-shaped.
func TestScrubQuotedKeyMarkerPrefixedValueIsNotProse(t *testing.T) {
	for _, in := range []string{
		`"api_key": [redacted-secret]actualcredential`,
		`"api_key": [redacted]actualcredential`,
		`'secret': [redacted-secret]actualcredential`,
	} {
		if got := Scrub(in); strings.Contains(got, "actualcredential") {
			t.Errorf("credential behind a marker prefix survived: in %q out %q", in, got)
		}
	}
	// The exact marker is still left alone (idempotence).
	for _, in := range []string{`"api_key": [redacted-secret]`, `"api_key": [redacted]`} {
		if got := Scrub(in); got != in {
			t.Errorf("an exact marker was re-wrapped: in %q out %q", in, got)
		}
	}
}
