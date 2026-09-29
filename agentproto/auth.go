package agentproto

import (
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/redactx"
)

// Auth material rides the transport, never the payload (§4.4). Sachin locked the
// Phase-3 model to a single bearer token = full access (no mTLS/OIDC/per-user).
// Phase 2 defines the seam and enforces nothing: over the unix socket the peer is
// trusted (filesystem perms are the auth, #1029), so BearerToken/TokenFrom* only
// EXTRACT a token — Phase 3 fills in the constant-time compare without reshaping a
// single message.
const (
	// AuthHeader is the REST + WS request header carrying the token.
	AuthHeader = "Authorization"
	// BearerScheme is the Authorization scheme prefix (note the trailing space).
	BearerScheme = "Bearer "
	// AccessTokenQueryParam is the WS query-param fallback. Browsers cannot set
	// request headers on a WebSocket handshake, so the token rides the URL for the
	// web client (§4.4); it must be part of the design now, not retrofitted.
	AccessTokenQueryParam = "access_token"
	accessTokenRedaction  = "REDACTED"
)

// RedactAccessTokenURL replaces every access_token value in raw while preserving
// the rest of the URL for diagnostics. url.URL.Redacted is not a substitute: it
// redacts userinfo only and leaves query parameters untouched.
func RedactAccessTokenURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		// An unparseable URL cannot be safely separated from its credential.
		return "[url redacted]"
	}
	redactAccessTokenQuery(parsed)
	// The raw-query scanner owns query grammar. The component sweep owns every
	// other parser-proven URI field, including nested percent encoding. Neither
	// pass depends on a match from the other before it runs.
	redactAccessTokenComponents(parsed)
	return parsed.String()
}

// redactAccessTokenQuery replaces every access_token value in u's raw query,
// using RawQuery as the authoritative representation. url.ParseQuery discards
// an entire ampersand-delimited chunk when it contains a semicolon or malformed
// escape, so its key enumeration is not safe for a redaction boundary (#4187).
func redactAccessTokenQuery(u *url.URL) {
	redacted, found := redactAccessTokenRawQuery(u.RawQuery)
	if found {
		u.RawQuery = redacted
	}
}

// redactAccessTokenComponents runs the URI-aware text matcher over every part
// of u that the query pass does not reach — most realistically the fragment,
// where an implicit-grant callback parks its token (#2771). A rootless URL
// carries its body in Opaque, and fields can appear in the path or userinfo.
// url.URL's string fields are a closed set; Scheme is the one field left out,
// because the parser rejects '=' in it.
func redactAccessTokenComponents(u *url.URL) {
	// u.Opaque is "encoded opaque data" (net/url/url.go:376) — unlike Path,
	// Fragment, and User, url.Parse does NOT percent-decode it.  We must decode
	// before scanning so that %61ccess_token= is matched; if the escape sequence
	// is malformed we must not emit the raw opaque value, so fall back to
	// redacting the whole field rather than leaving a credential in place.
	// Write back ONLY when the scan actually redacted something. url.URL.String
	// prints Opaque verbatim — there is no RawOpaque to re-escape from, unlike
	// the Path/RawPath pair below — so storing the decoded form unconditionally
	// rewrote every opaque URL that passed through, credential or not:
	// mailto:user%40host.example became mailto:user@host.example, and
	// af:a%2Fb%20c became af:a/b c, which is no longer a valid URL (#4161).
	// Leaving the original encoded bytes in place when nothing matched keeps
	// this a redactor rather than a normalizer.
	if _, malformed := redactx.PercentDecode(u.Opaque, false); malformed {
		u.Opaque = accessTokenRedaction
	} else if redacted, found := redactPercentEncodedAccessTokenText(u.Opaque, false); found {
		// Source mapping keeps every non-sensitive escape in its original form.
		u.Opaque = redacted
	}
	// Mirror the Path/Fragment branches below: scan u.Opaque (the bytes
	// url.URL.String will print — Opaque carries no Raw* twin, so this is the
	// decoded-pass output verbatim, not an Escaped* re-encoding) for an
	// access_token= overlap the decoded view cannot see, exactly as documented
	// there. Run unconditionally rather than gating on the decoded sweep
	// missing: a co-located opaque component can carry both a %HH-overlapped
	// access_token=<secret> and a later literal access_token=<value>, where
	// the single-anchor decoded sweep anchors at the trailing literal and a
	// gate would short-circuit this raw scan for the whole component. The
	// decoded pass's redacted span is the literal REDACTED marker, which
	// contains no access_token= needle, so idempotency (not a code-path gate)
	// keeps this from reprocessing a span the decoded sweep already redacted.
	if rawRedacted, found := redactRawAccessTokenValue(u.Opaque, "/;?#"); found {
		u.Opaque = rawRedacted
	}
	u.Host = RedactAccessTokenText(u.Host)
	if path, found := redactPercentEncodedAccessTokenText(u.Path, false); found {
		// RawPath is honoured only while it still encodes Path, and a rewritten
		// Path leaves it stale. Drop it so String re-escapes from the redacted
		// value rather than reprinting the credential it was holding.
		u.Path, u.RawPath = path, ""
	}
	// Scan the bytes String() will print for an access_token= overlap the
	// decoded view cannot see. A valid %HH escape (e.g. %ac) can overlap the
	// leading characters of an otherwise literal access_token<...> substring,
	// collapsing access_token= out of the decoded u.Path while the raw bytes
	// String() will emit still carry a literal access_token=<value>. Run
	// unconditionally on the decoded-pass output rather than gating on the
	// decoded sweep missing: a co-located component can carry both such an
	// overlap and a later literal access_token=, where the single-anchor
	// decoded sweep anchors at the trailing literal and a gate would
	// short-circuit this raw scan for the whole component. The decoded pass's
	// redacted span is the literal REDACTED marker, which contains no
	// access_token= needle, so idempotency keeps this from reprocessing a span
	// the decoded sweep already redacted.
	//
	// EscapedPath is the authoritative view of what String() will print:
	// when url.Parse found the raw path was already a canonical encoding
	// of Path it drops RawPath (sets it to "") and EscapedPath re-encodes
	// from Path; when RawPath is set EscapedPath honours it. Either way it
	// sees the decoded pass's rewritten bytes (if any) plus any overlap the
	// decoded scan did not reach. After a redaction, set both RawPath (the
	// new verbatim form) and Path (its unescape) so EscapedPath's validity
	// check keeps honouring RawPath.
	if rawRedacted, found := redactRawAccessTokenValue(u.EscapedPath(), "/;?#"); found {
		u.RawPath = rawRedacted
		if unescaped, err := url.PathUnescape(rawRedacted); err == nil {
			u.Path = unescaped
		}
	}
	if fragment, found := redactPercentEncodedAccessTokenText(u.Fragment, false); found {
		u.Fragment, u.RawFragment = fragment, ""
	}
	// Mirrors the Path branch above. The raw access_token= overlap scan runs
	// unconditionally on the decoded-pass output (idempotent via the REDACTED
	// marker, which contains no access_token= needle), not gated on the
	// decoded sweep missing. EscapedFragment is the authoritative view of
	// what String() will print: url.Parse clears RawFragment when the raw
	// was already canonical, in which case EscapedFragment re-encodes from
	// Fragment; otherwise it honours RawFragment. PathUnescape matches the
	// encodeFragment unescape: + survives as + in either mode, and only
	// %HH escapes are folded.
	if rawRedacted, found := redactRawAccessTokenValue(u.EscapedFragment(), "/;?#"); found {
		u.RawFragment = rawRedacted
		if unescaped, err := url.PathUnescape(rawRedacted); err == nil {
			u.Fragment = unescaped
		}
	}
	if u.User != nil {
		u.User = redactAccessTokenUserinfo(u.User)
	}
}

// redactAccessTokenUserinfo returns user with any access_token field redacted
// out of its name or password, and returns user itself when there is none — an
// untouched Userinfo reprints exactly as it parsed.
func redactAccessTokenUserinfo(user *url.Userinfo) *url.Userinfo {
	name, _ := redactPercentEncodedAccessTokenText(user.Username(), false)
	password, hasPassword := user.Password()
	redactedPassword, _ := redactPercentEncodedAccessTokenText(password, false)
	var result *url.Userinfo
	switch {
	case name == user.Username() && redactedPassword == password:
		result = user
	case !hasPassword:
		result = url.User(name)
	default:
		result = url.UserPassword(name, redactedPassword)
	}
	// Mirror the Path/Fragment branches above: scan Userinfo.String (the
	// bytes url.URL.String will print for the userinfo — it calls ui.String()
	// directly, mirroring EscapedPath/EscapedFragment) for an access_token=
	// overlap the decoded view cannot see. A valid %HH escape (e.g. %ac)
	// can overlap the leading characters of an otherwise literal
	// access_token<...> substring, collapsing access_token= out of the
	// decoded Username()/Password() view while the serialized bytes still
	// carry a literal access_token=<value>. Run unconditionally on the
	// decoded-pass output (idempotent via the REDACTED marker, which
	// contains no access_token= needle, rather than gated on the decoded
	// sweep missing): userinfo is part of this function's component-wide
	// redaction contract — the same leak that motivated the
	// opaque/path/fragment branches above applies to userinfo too, even
	// though no current in-repo production caller happens to route an
	// overlapping userinfo through it.
	//
	// Userinfo.String joins the name and password with a single ":" (and
	// escapes any literal ":" in either field as %3A), so ":" is the only
	// value terminator in the serialized form: an access_token= span in
	// the name ends at the ":" separator (or at the end of the string when
	// there is no password), and one in the password — the trailing field —
	// runs to the end of the string. Re-parse the redacted bytes through
	// net/url so the result stores the parser-decoded name/password that
	// Userinfo.String re-encodes back via encodeUserinfo, the same way
	// url.User and url.UserPassword above store the decoded forms they
	// were constructed with.
	serialized := result.String()
	rawRedacted, found := redactRawAccessTokenValue(serialized, ":")
	if !found {
		return result
	}
	reparsed, err := url.Parse("http://" + rawRedacted + "@")
	if err != nil || reparsed.User == nil {
		// A parse failure means the raw scan left bytes net/url can no
		// longer parse as userinfo. redactRawAccessTokenValue only
		// substitutes a value span with the REDACTED marker — never a
		// userinfo-grammar-reserved byte — so this branch is unreachable
		// for inputs the original parse produced; fail closed regardless
		// rather than emit a credential the decoded pass could not see.
		return url.User(accessTokenRedaction)
	}
	return reparsed.User
}

// RedactAccessTokenError strips an access_token from a failed request while
// retaining the original error chain whenever structured redaction is enough.
// net/http and WebSocket dial failures commonly contain a *url.Error whose URL
// includes the full query string.
func RedactAccessTokenError(err error, token string) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		urlErr.URL = RedactAccessTokenURL(urlErr.URL)
	}

	message := redactAccessTokenTextOutsideStructuredURL(err.Error(), urlErr)
	if token != "" {
		message = strings.ReplaceAll(message, token, accessTokenRedaction)
	}
	if message != err.Error() {
		// A non-URL error carried the credential somewhere the structured pass
		// could not reach. Drop its type rather than retain the secret.
		return errors.New(message)
	}
	return err
}

// RedactAccessTokenText is the logging-boundary backstop for an access_token
// field embedded in otherwise unstructured text. Call sites that know they are
// handling a URL or request error must still use the structured helpers above.
// Percent escapes are ordinary bytes here: only RedactAccessTokenURL has parser
// provenance that permits decoding them without rewriting arbitrary prose.
//
// Every occurrence of the field name is redacted, whatever precedes it. The
// match used to be gated on a hand-listed set of separators the field may follow
// (`?&;` and whitespace), which made the default answer for an unlisted byte
// "leave the credential alone": ';' had to be added in #2687 and '#' in #2771,
// and the byte after that would have been the third fix. Matching a longer name
// like "my_access_token=" now costs a diagnostic field whose value is a
// credential anyway — the same trade the value scan below already makes, in the
// same direction.
func RedactAccessTokenText(text string) string {
	spans := accessTokenTextValueSpans(text)
	if len(spans) == 0 {
		return text
	}
	return replaceAccessTokenTextSpans(text, spans)
}

func redactAccessTokenTextOutsideStructuredURL(text string, urlErr *url.Error) string {
	if urlErr == nil {
		return RedactAccessTokenText(text)
	}

	structured := urlErr.Error()
	quotedURL := strconv.Quote(urlErr.URL)
	structuredPrefix := urlErr.Op + " " + quotedURL + ": "
	if !strings.HasPrefix(structured, structuredPrefix) ||
		strings.Count(text, structured) != 1 {
		// A custom wrapper changed the nested error's text, so there is no
		// unique structured occurrence whose provenance is safe to exempt.
		return RedactAccessTokenText(text)
	}
	structuredStart := strings.Index(text, structured)
	urlOffset := len(urlErr.Op) + 1
	urlStart := structuredStart + urlOffset
	urlEnd := urlStart + len(quotedURL)
	return RedactAccessTokenText(text[:urlStart]) +
		text[urlStart:urlEnd] +
		RedactAccessTokenText(text[urlEnd:])
}

func indexFoldASCII(text, lowerASCII string) int {
	for i := 0; i+len(lowerASCII) <= len(text); i++ {
		matches := true
		for j := range lowerASCII {
			got := text[i+j]
			if got >= 'A' && got <= 'Z' {
				got += 'a' - 'A'
			}
			if got != lowerASCII[j] {
				matches = false
				break
			}
		}
		if matches {
			return i
		}
	}
	return -1
}

// accessTokenValueEnd reports whether char is an unambiguous end of an
// access_token value. Whitespace and quotes close a field in any text, and '#'
// opens a URL fragment — which RedactAccessTokenURL hands back to this scan as
// its own string, so a fragment token is still redacted rather than folded into
// the query one.
//
// Every other byte counts as credential material, including the '&' and ';'
// separators an upstream parser may or may not honour (#2690). That default is
// the reason this half of the scan has never leaked: an unfamiliar delimiter
// costs the neighbouring field, never the token.
func accessTokenValueEnd(char byte) bool {
	return strings.ContainsRune(" \t\r\n#\"'", rune(char))
}

// BearerToken extracts the token from an Authorization header value, matching the
// scheme case-insensitively. It returns "" when the value is absent or not a
// bearer credential. No validation or enforcement — that is Phase 3.
func BearerToken(headerValue string) string {
	if len(headerValue) < len(BearerScheme) {
		return ""
	}
	if !strings.EqualFold(headerValue[:len(BearerScheme)], BearerScheme) {
		return ""
	}
	return strings.TrimSpace(headerValue[len(BearerScheme):])
}

// AccessTokenFromQuery reads the ?access_token= WS/browser fallback from parsed
// query values, returning "" when absent.
func AccessTokenFromQuery(q url.Values) string {
	return q.Get(AccessTokenQueryParam)
}
