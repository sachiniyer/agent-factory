package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebTabProxyRejectsNonLoopbackTargetRedactsOverlapPercentEscape is the
// end-to-end emitter smoke for daemon/webtab_serve.go:296's
// `agentproto.RedactAccessTokenURL(target.URL)` — the 400 "web tab target is
// not loopback" error body. It mirrors the existing
// TestWebTabProxy_RejectsNonLoopbackTarget but drives the URL shape the
// decode-based redactor missed before the raw-scan fix: a valid %HH escape
// (%ac) whose hex digits overlap the leading characters of an otherwise-literal
// `access_token<...>` substring. Without the raw-bytes scan fallback in
// redactAccessTokenQueryPair, the 400 body the daemon returns would contain the
// literal `access_token=<value>` substring verbatim because net/url re-emits
// RawQuery on the structured-sweep miss.
func TestWebTabProxyRejectsNonLoopbackTargetRedactsOverlapPercentEscape(t *testing.T) {
	const secret = "af-sentinel-webtab-loopback-overlap"
	target := "https://example.com/?%access_token=" + secret + "&view=2"
	mux, id, tabID := newWebTabProxyFixture(t, target)

	rec := proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("external overlap target: status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Errorf("400 body leaked the overlap token value: %q", body)
	}
	if !strings.Contains(body, "access_token=REDACTED") {
		t.Errorf("400 body missing the redaction marker; got %q", body)
	}
}

// TestWebTabProxyFailureLogsRedactedOverlapPercentEscape is the end-to-end
// emitter smoke for daemon/webtab_serve.go:538's
// `agentproto.RedactAccessTokenURL(targetURL.String())` — the 502
// proxy-failure WarningLog line. It mirrors the existing
// TestWebTabProxyFailureDoesNotLogTargetAccessToken but uses the overlap %HH
// shape that defeated the decode-based redactor: a valid %ac over the leading
// characters of an otherwise-literal `access_token<...>`. Without the raw-scan
// fallback the warning log would persist the literal `access_token=<value>`
// substring verbatim, exactly as it does for the literal-key case the existing
// test guards.
func TestWebTabProxyFailureLogsRedactedOverlapPercentEscape(t *testing.T) {
	const secret = "af-sentinel-webtab-proxylog-overlap"
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	targetURL := dead.URL + "/app?view=2&%access_token=" + secret
	dead.Close()

	warnings := captureWarnings(t)

	mux, sessionID, tabID := newWebTabProxyFixture(t, targetURL)
	rec := proxyGet(t, mux, sessionID, tabID, "app?view=2")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if strings.Contains(warnings.String(), secret) {
		t.Fatalf("web-tab proxy log exposed the overlap token value: %s", warnings.String())
	}
	if !strings.Contains(warnings.String(), "access_token=REDACTED") {
		t.Fatalf("web-tab proxy log lost its redaction marker: %s", warnings.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("web-tab proxy error response exposed the overlap token value: %s", rec.Body.String())
	}
}
