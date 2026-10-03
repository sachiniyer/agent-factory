package agentproto

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// extractAccessTokenValue returns the bytes after the first "access_token=" in s
// up to the next access_token value terminator, so a test can inspect exactly
// what the redactor left in the credential position. The terminators mirror
// accessTokenValueEnd (whitespace, #, and quotes close a field in prose) plus '&'
// (which closes a query pair in a URL), so the helper works for both a quoted
// *url.Error and a bare prose mention. The marker-corruption bug re-injects into
// that position: a correct redaction leaves accessTokenRedaction there and
// nothing more, while the corruption leaves accessTokenRedaction plus a
// marker-derived tail (e.g. "REDACTEDACTED" for token "RED"). Asserting the value
// equals the marker exactly — rather than strings.HasPrefix(value, token) —
// distinguishes the re-injection from the fact that the marker itself starts
// with any of its own prefixes.
func extractAccessTokenValue(s string) string {
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

// accessTokenMarkerSubstrings returns every non-empty substring of
// accessTokenRedaction exactly once. The re-injection fires for any token that
// strings.ReplaceAll can match *inside* the 8-byte marker, which is every
// substring of "REDACTED" — not only the prefixes the bug report catalogued.
// Covering the whole set pins the boundary the redactor must hold for any
// secret shape that happens to be a fragment of its own marker.
func accessTokenMarkerSubstrings() []string {
	seen := make(map[string]struct{})
	var out []string
	for i := 0; i < len(accessTokenRedaction); i++ {
		for j := i + 1; j <= len(accessTokenRedaction); j++ {
			sub := accessTokenRedaction[i:j]
			if _, ok := seen[sub]; ok {
				continue
			}
			seen[sub] = struct{}{}
			out = append(out, sub)
		}
	}
	return out
}

// TestRedactAccessTokenErrorNeverExtendsMarker is the regression test for the
// access_token redaction marker-corruption bug. The literal-token catch-all in
// RedactAccessTokenError runs after the URL and text passes have already
// substituted every access_token= value with the literal "REDACTED". A bare
// strings.ReplaceAll keyed on the secret matches that marker whenever the
// secret is a substring of "REDACTED", rewriting the marker into the marker
// plus its own tail and re-injecting the secret bytes immediately after
// access_token=. The redacted value must always be exactly the marker.
//
// The table covers two reachability shapes for every substring of the marker:
//
//   - a *url.Error whose URL query is access_token=<token>, exercising the path
//     the WebSocket dialers and the web-tab reverse proxy hit (the URL pass
//     writes the marker, then the catch-all runs), and
//   - a plain error whose message embeds access_token=<token> in prose,
//     exercising the text-pass fallback (redactAccessTokenTextOutsideStructuredURL
//     with a nil *url.Error writes the marker, then the catch-all runs).
//
// Both were observed corrupting before the fix; both must hold the marker
// verbatim after it.
func TestRedactAccessTokenErrorNeverExtendsMarker(t *testing.T) {
	for _, token := range accessTokenMarkerSubstrings() {
		token := token
		t.Run("url/"+token, func(t *testing.T) {
			dialErr := &url.Error{
				Op:  "Get",
				URL: "ws://box:8080/stream?access_token=" + token,
				Err: errors.New("dial failed"),
			}
			got := RedactAccessTokenError(dialErr, token).Error()
			if outTok := extractAccessTokenValue(got); outTok != accessTokenRedaction {
				t.Errorf("token %q (url): access_token value = %q, want exactly %q (marker extended); full: %s",
					token, outTok, accessTokenRedaction, got)
			}
		})
		t.Run("text/"+token, func(t *testing.T) {
			plain := errors.New("proxy to http://localhost:3000/app?access_token=" + token + " failed: connection refused")
			got := RedactAccessTokenError(plain, token).Error()
			if outTok := extractAccessTokenValue(got); outTok != accessTokenRedaction {
				t.Errorf("token %q (text): access_token value = %q, want exactly %q (marker extended); full: %s",
					token, outTok, accessTokenRedaction, got)
			}
		})
	}
}

// TestRedactAccessTokenErrorCatchAllStillRedactsLiteral guarantees the
// marker-safe catch-all did not become a no-op: a secret that appears somewhere
// the structured and text passes cannot reach (here, a bare token mention in
// non-access_token prose) is still replaced with the marker, while an existing
// marker in the same message is left verbatim. The first case uses a token that
// is not a substring of the marker (the realistic 43-char-token shape), the
// second uses a marker-substring token so the test would catch a regression that
// either stops redacting legitimate occurrences or re-corrupts the co-located
// marker.
func TestRedactAccessTokenErrorCatchAllStillRedactsLiteral(t *testing.T) {
	t.Run("non-substring token in prose", func(t *testing.T) {
		const token = "af-sentinel-literal-catchall"
		plain := errors.New("transport failure carrying token " + token)
		got := RedactAccessTokenError(plain, token).Error()
		if strings.Contains(got, token) {
			t.Errorf("literal token survived redaction: %s", got)
		}
		if !strings.Contains(got, accessTokenRedaction) {
			t.Errorf("literal token was not replaced with the marker: %s", got)
		}
	})
	t.Run("marker-substring token redacts prose and preserves co-located marker", func(t *testing.T) {
		const token = "RED"
		// One marker already written by the text pass (access_token=RED),
		// plus a second bare mention the catch-all owns ("saw RED here").
		plain := errors.New("access_token=" + token + " and saw " + token + " here")
		got := RedactAccessTokenError(plain, token).Error()
		if outTok := extractAccessTokenValue(got); outTok != accessTokenRedaction {
			t.Errorf("co-located marker corrupted: value = %q, want %q; full: %s",
				outTok, accessTokenRedaction, got)
		}
		if !strings.Contains(got, "saw "+accessTokenRedaction+" here") {
			t.Errorf("bare literal mention was not redacted: %s", got)
		}
		// The marker must appear exactly twice: once for the access_token= value
		// the text pass redacted, once for the bare mention the catch-all owns.
		// Zero means the token survived; not-two means the catch-all either
		// failed to redact the bare mention or duplicated the existing marker by
		// matching inside it.
		if n := strings.Count(got, accessTokenRedaction); n != 2 {
			t.Errorf("want exactly two markers in %q, got %d", got, n)
		}
	})
}

// TestRedactAccessTokenErrorPrefixTokenPreservesStructuredURLContext is the
// regression guard against reordering the literal pass ahead of the structured
// pass (the bug report's suggested fix). That reorder breaks the structured-URL
// preservation path when a URL carries a second access_token whose value differs
// from the passed token (the existing TestRedactAccessTokenErrorRedactsOverlapHost
// shape): the literal pass only redacts the passed token, so the message's URL
// no longer matches the fully-redacted *url.Error, the structured path falls
// back to a full-text scan, and the URL collapses to a single marker. This test
// pins that the URL context survives even when the passed token is a marker
// substring, so a future reorder cannot silently regress it.
func TestRedactAccessTokenErrorPrefixTokenPreservesStructuredURLContext(t *testing.T) {
	const hostTok = "RED" // a marker prefix — the corruption-triggering shape
	const queryTok = "af-sentinel-query-distinct"
	dialErr := &url.Error{
		Op:  "Get",
		URL: "ws://access_token=" + hostTok + ":8443/v1/sessions/test/stream?access_token=" + queryTok,
		Err: errors.New("dial tcp: no such host"),
	}
	got := RedactAccessTokenError(dialErr, hostTok).Error()
	wantURL := "ws://access_token=REDACTED:8443/v1/sessions/test/stream?access_token=REDACTED"
	if !strings.Contains(got, wantURL) {
		t.Errorf("structured URL context collapsed or lost; got %s\nwant to contain %q", got, wantURL)
	}
	if strings.Contains(got, queryTok) {
		t.Errorf("query token %q leaked: %s", queryTok, got)
	}
	// wantURL pins both access_token markers to exactly "REDACTED": a re-extended
	// marker (e.g. access_token=REDACTEDACTED:8443) or a collapsed single marker
	// (the reorder regression) both fail the substring above. Sanity-check the
	// marker count here so the failure mode names itself when it regresses.
	if n := strings.Count(got, accessTokenRedaction); n != 2 {
		t.Errorf("want exactly two markers (host + query) in %q, got %d", got, n)
	}
}

// TestRedactAccessTokenErrorRetainsChainForMarkerSubstringToken confirms the
// "retain the original error chain whenever structured redaction is enough"
// half of RedactAccessTokenError's docstring still holds when the token is a
// marker substring: a *url.Error whose only credential is the URL's access_token
// is redacted entirely by the URL pass, so the catch-all is a no-op and the
// original error (and its wrapped cause) is returned verbatim. A regression
// that made the catch-all rewrite the message for marker-substring tokens would
// drop the error chain on the dialer paths.
func TestRedactAccessTokenErrorRetainsChainForMarkerSubstringToken(t *testing.T) {
	for _, token := range []string{"R", "RED", "REDACT", "REDACTE"} {
		token := token
		t.Run(token, func(t *testing.T) {
			cause := errors.New("connection refused")
			dialErr := &url.Error{
				Op:  "Get",
				URL: "ws://box:8080/stream?tab=2&access_token=" + token,
				Err: cause,
			}
			got := RedactAccessTokenError(dialErr, token)
			if !errors.Is(got, cause) {
				t.Errorf("token %q: structured redaction lost the original error chain: %v", token, got)
			}
			gotStr := got.Error()
			if outTok := extractAccessTokenValue(gotStr); outTok != accessTokenRedaction {
				t.Errorf("token %q: access_token value = %q, want exactly %q (marker extended); full: %s",
					token, outTok, accessTokenRedaction, gotStr)
			}
		})
	}
}

// TestRedactAccessTokenLiteralOutsideMarkersIsIndependentOfMarkerContext is a
// unit test for the helper itself, decoupled from the URL/text passes. It
// asserts the two contracts the helper holds: an existing marker is never
// modified or duplicated regardless of the token's shape, and a bare token
// occurrence in marker-free text is replaced exactly as strings.ReplaceAll
// would replace it.
func TestRedactAccessTokenLiteralOutsideMarkersIsIndependentOfMarkerContext(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		token string
		want  string
	}{
		{
			name:  "no marker, token replaced verbatim",
			text:  "carry RED in prose",
			token: "RED",
			want:  "carry REDACTED in prose",
		},
		{
			name:  "marker preserved, surrounding token replaced",
			text:  "access_token=REDACTED saw RED here",
			token: "RED",
			want:  "access_token=REDACTED saw REDACTED here",
		},
		{
			name:  "full-marker token is identity",
			text:  "access_token=REDACTED and REDACTED again",
			token: "REDACTED",
			want:  "access_token=REDACTED and REDACTED again",
		},
		{
			name:  "single-char prefix token preserves marker",
			text:  "access_token=REDACTED",
			token: "R",
			want:  "access_token=REDACTED",
		},
		{
			name:  "non-prefix substring token preserves marker",
			text:  "access_token=REDACTED",
			token: "DACT",
			want:  "access_token=REDACTED",
		},
		{
			name:  "empty token is identity",
			text:  "access_token=REDACTED and RED",
			token: "",
			want:  "access_token=REDACTED and RED",
		},
		{
			name:  "right-straddling token redacts suffix past marker",
			text:  "err: REDACTEDfoo end",
			token: "EDfoo",
			want:  "err: REDACTEDREDACTED end",
		},
		{
			name:  "left-straddling token redacts prefix before marker",
			text:  "err: fooREDACTED end",
			token: "fooRED",
			want:  "err: REDACTEDREDACTED end",
		},
		{
			name:  "straddling token inside coincidental marker is redacted",
			text:  "REDACTEDfoo",
			token: "EDfoo",
			want:  "REDACTEDREDACTED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactAccessTokenLiteralOutsideMarkers(tc.text, tc.token); got != tc.want {
				t.Errorf("redactAccessTokenLiteralOutsideMarkers(%q, %q) = %q, want %q",
					tc.text, tc.token, got, tc.want)
			}
		})
	}
}

// TestRedactAccessTokenLiteralOutsideMarkersStraddleRedactsCoincidentalMarker
// is the regression test for the straddle case a naive split-on-marker
// implementation misses: a token that begins inside a coincidental "REDACTED"
// (not one the earlier passes wrote) and ends in the following segment. The
// split approach confines the literal pass to segments between markers, so
// neither segment contains the whole token and the secret survives. The scan
// redacts the suffix past the marker, restoring the whole-message replacement
// guarantee for arbitrary transport error text the catch-all was originally
// written to cover.
func TestRedactAccessTokenLiteralOutsideMarkersStraddleRedactsCoincidentalMarker(t *testing.T) {
	// Right straddle: token starts in the marker suffix, ends past it.
	// "EDfoo" begins at the "E" of "REDACTED" (index 6) and ends in "foo".
	if got := redactAccessTokenLiteralOutsideMarkers("REDACTEDfoo", "EDfoo"); strings.Contains(got, "EDfoo") {
		t.Errorf("right straddle: token survived in %q", got)
	}
	if got := redactAccessTokenLiteralOutsideMarkers("REDACTEDfoo", "EDfoo"); !strings.Contains(got, "REDACTED") {
		t.Errorf("right straddle: marker lost in %q", got)
	}
	// Left straddle: token starts before the marker, ends inside it.
	// "fooRED" ends at the "D" of "REDACTED" (index 8), starts in "foo".
	if got := redactAccessTokenLiteralOutsideMarkers("fooREDACTED", "fooRED"); strings.Contains(got, "fooRED") {
		t.Errorf("left straddle: token survived in %q", got)
	}
	if got := redactAccessTokenLiteralOutsideMarkers("fooREDACTED", "fooRED"); !strings.Contains(got, "REDACTED") {
		t.Errorf("left straddle: marker lost in %q", got)
	}
}

// TestRedactAccessTokenLiteralOutsideMarkersTokenContainingMarker is the
// regression test for the inverse of the marker-substring case: a token that
// *contains* the whole marker internally. The earlier scan emitted the
// embedded marker verbatim and redacted the token's other bytes separately,
// which reconstructed the token across the two emitted markers (a token equal
// to the marker plus a suffix, e.g. "REDACTEDR", produced "REDACTEDREDACTED"
// which still contains the credential contiguously) or left the token's suffix
// past the marker unredacted (a token with a marker in the middle, e.g.
// "prefixREDACTEDsecret", produced "REDACTEDREDACTEDsecret"). When the token
// contains the marker the embedded marker is part of the secret, so the scan
// must replace the whole token with a single marker. The bare-token shape is
// the one RedactAccessTokenError reaches for an unstructured error whose
// message is the token itself.
func TestRedactAccessTokenLiteralOutsideMarkersTokenContainingMarker(t *testing.T) {
	// Token equal to the marker plus a suffix: the right-straddle branch used
	// to preserve the leading marker and emit a second marker, reconstructing
	// the credential across the boundary.
	t.Run("marker plus suffix token", func(t *testing.T) {
		token := "REDACTEDR"
		got := redactAccessTokenLiteralOutsideMarkers(token, token)
		if strings.Contains(got, token) {
			t.Errorf("token %q reconstructed in output %q", token, got)
		}
		if got != accessTokenRedaction {
			t.Errorf("token %q: got %q, want a single marker %q", token, got, accessTokenRedaction)
		}
	})
	// Token with the marker in the middle: the left-straddle branch used to
	// redact only the prefix before the marker, emit the marker verbatim, and
	// leave the suffix past the marker unredacted.
	t.Run("marker in middle of token", func(t *testing.T) {
		token := "prefixREDACTEDsecret"
		got := redactAccessTokenLiteralOutsideMarkers(token, token)
		if strings.Contains(got, token) {
			t.Errorf("token %q survived in output %q", token, got)
		}
		if strings.Contains(got, "secret") {
			t.Errorf("token suffix %q survived in output %q", "secret", got)
		}
		if got != accessTokenRedaction {
			t.Errorf("token %q: got %q, want a single marker %q", token, got, accessTokenRedaction)
		}
	})
	// Token containing the marker with surrounding prose: the token occurrence
	// is redacted wholesale while an unrelated real marker elsewhere in the
	// text is preserved verbatim (the scan must still not match a token inside
	// a marker it did not write).
	t.Run("marker-containing token redacted, co-located real marker preserved", func(t *testing.T) {
		token := "REDACTEDR"
		text := "access_token=REDACTED and saw " + token + " here"
		got := redactAccessTokenLiteralOutsideMarkers(text, token)
		if strings.Contains(got, token) {
			t.Errorf("token %q survived in %q", token, got)
		}
		if outTok := extractAccessTokenValue(got); outTok != accessTokenRedaction {
			t.Errorf("co-located marker corrupted: value = %q, want %q; full: %s",
				outTok, accessTokenRedaction, got)
		}
		if !strings.Contains(got, "saw "+accessTokenRedaction+" here") {
			t.Errorf("token occurrence was not redacted: %s", got)
		}
	})
}

// TestRedactAccessTokenErrorTokenContainingMarker is the end-to-end regression
// for the same token-contains-marker shape through RedactAccessTokenError: an
// unstructured error whose message is the bare token must not reconstruct the
// credential across two markers or leave a suffix unredacted.
func TestRedactAccessTokenErrorTokenContainingMarker(t *testing.T) {
	for _, token := range []string{"REDACTEDR", "prefixREDACTEDsecret"} {
		token := token
		t.Run(token, func(t *testing.T) {
			got := RedactAccessTokenError(errors.New(token), token).Error()
			if strings.Contains(got, token) {
				t.Errorf("token %q survived in redacted error: %q", token, got)
			}
			if strings.Count(got, accessTokenRedaction) != 1 {
				t.Errorf("token %q: want exactly one marker, got %q (count %d)",
					token, got, strings.Count(got, accessTokenRedaction))
			}
		})
	}
}
