package agentproto

import (
	"errors"
	"net/http"
	"net/url"
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

// RedactAccessTokenURL replaces every access_token query value in raw while
// preserving the rest of the URL for diagnostics. url.URL.Redacted is not a
// substitute: it redacts userinfo only and leaves query parameters untouched.
func RedactAccessTokenURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		// An unparseable URL cannot be safely separated from its credential.
		return "[url redacted]"
	}
	q := parsed.Query()
	if _, ok := q[AccessTokenQueryParam]; !ok {
		// URL.Query discards malformed query pairs. The text pass still catches
		// an exact access_token field without trusting a partially parsed query.
		return RedactAccessTokenText(raw)
	}
	q.Set(AccessTokenQueryParam, accessTokenRedaction)
	parsed.RawQuery = q.Encode()
	return parsed.String()
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

	message := RedactAccessTokenText(err.Error())
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
// query embedded in otherwise unstructured text. Call sites that know they are
// handling a URL or request error must still use the structured helpers above.
func RedactAccessTokenText(text string) string {
	needle := AccessTokenQueryParam + "="
	if !strings.Contains(text, needle) {
		return text
	}

	var redacted strings.Builder
	rest := text
	for {
		i := strings.Index(rest, needle)
		if i < 0 {
			redacted.WriteString(rest)
			return redacted.String()
		}
		valueStart := i + len(needle)
		if i > 0 && rest[i-1] != '?' && rest[i-1] != '&' && rest[i-1] != ';' {
			redacted.WriteString(rest[:valueStart])
			rest = rest[valueStart:]
			continue
		}

		valueEnd := valueStart
		for valueEnd < len(rest) && !strings.ContainsRune("&; \t\r\n#\"'", rune(rest[valueEnd])) {
			valueEnd++
		}
		redacted.WriteString(rest[:valueStart])
		redacted.WriteString(accessTokenRedaction)
		rest = rest[valueEnd:]
	}
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
