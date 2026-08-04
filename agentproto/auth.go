package agentproto

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	if !redactAccessTokenQuery(parsed) {
		// URL.Query discards malformed query pairs, so a structured miss says
		// nothing about the raw text. The text pass reads the whole string
		// rather than trusting a partially parsed query.
		return RedactAccessTokenText(raw)
	}
	redactAccessTokenComponents(parsed)
	return parsed.String()
}

// redactAccessTokenQuery replaces every access_token value in u's parsed query,
// reporting whether the query carried one at all. Working through url.Values is
// what keeps the neighbouring parameters readable: their separators are known to
// be separators here, which is exactly the fact unstructured text lacks.
func redactAccessTokenQuery(u *url.URL) bool {
	q := u.Query()
	found := false
	for key := range q {
		if strings.EqualFold(key, AccessTokenQueryParam) {
			q.Set(key, accessTokenRedaction)
			found = true
		}
	}
	if !found {
		return false
	}
	u.RawQuery = q.Encode()
	return true
}

// redactAccessTokenComponents runs the text backstop over every part of u that
// the query pass does not reach — most realistically the fragment, where an
// implicit-grant callback parks its token (#2771), but a rootless URL carries
// its whole body in Opaque and nothing stops a field from appearing in the path
// or the userinfo either. url.URL's string fields are a closed set, so covering
// them is a claim that stays true; covering "the separators a token can hide
// behind" is the open-ended guess that missed ';' in #2687 and '#' in #2771.
// Scheme is the one field left out, because the parser rejects '=' in it.
//
// Re-running the text pass over the serialized URL instead would defeat the
// query pass: '&' does not end a value, so the scan would swallow every
// parameter behind the one it just redacted.
func redactAccessTokenComponents(u *url.URL) {
	u.Opaque = RedactAccessTokenText(u.Opaque)
	u.Host = RedactAccessTokenText(u.Host)
	if path := RedactAccessTokenText(u.Path); path != u.Path {
		// RawPath is honoured only while it still encodes Path, and a rewritten
		// Path leaves it stale. Drop it so String re-escapes from the redacted
		// value rather than reprinting the credential it was holding.
		u.Path, u.RawPath = path, ""
	}
	if fragment := RedactAccessTokenText(u.Fragment); fragment != u.Fragment {
		u.Fragment, u.RawFragment = fragment, ""
	}
	if u.User != nil {
		u.User = redactAccessTokenUserinfo(u.User)
	}
}

// redactAccessTokenUserinfo returns user with any access_token field redacted
// out of its name or password, and returns user itself when there is none — an
// untouched Userinfo reprints exactly as it parsed.
func redactAccessTokenUserinfo(user *url.Userinfo) *url.Userinfo {
	name := RedactAccessTokenText(user.Username())
	password, hasPassword := user.Password()
	redactedPassword := RedactAccessTokenText(password)
	if name == user.Username() && redactedPassword == password {
		return user
	}
	if !hasPassword {
		return url.User(name)
	}
	return url.UserPassword(name, redactedPassword)
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
	needle := AccessTokenQueryParam + "="
	if indexFoldASCII(text, needle) < 0 {
		return text
	}

	var redacted strings.Builder
	rest := text
	for {
		i := indexFoldASCII(rest, needle)
		if i < 0 {
			redacted.WriteString(rest)
			return redacted.String()
		}
		valueStart := i + len(needle)
		valueEnd := valueStart
		for valueEnd < len(rest) && !accessTokenValueEnd(rest[valueEnd]) {
			valueEnd++
		}
		redacted.WriteString(rest[:valueStart])
		redacted.WriteString(accessTokenRedaction)
		rest = rest[valueEnd:]
	}
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

// TokenFromRequest extracts the bearer token an incoming REST or WS request
// presents, preferring the Authorization header and falling back to the
// ?access_token= query param (the browser WS path). It returns "" when neither is
// present. Pure extraction; the caller's auth middleware decides what to do with
// it (a no-op in Phase 2).
func TokenFromRequest(r *http.Request) string {
	if tok := BearerToken(r.Header.Get(AuthHeader)); tok != "" {
		return tok
	}
	if r.URL != nil {
		return AccessTokenFromQuery(r.URL.Query())
	}
	return ""
}
