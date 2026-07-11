package agentproto

import (
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
)

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
