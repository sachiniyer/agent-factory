package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebTabProxyRejectsNonLoopbackTargetRedactsInNeedlePercentEscape is the
// in-needle-escape sibling of
// TestWebTabProxyRejectsNonLoopbackTargetRedactsOverlapPercentEscape. It
// drives the 400 "web tab target is not loopback" error body at
// daemon/webtab_serve.go:296 (`agentproto.RedactAccessTokenURL(target.URL)`)
// with the combined shape the #4663 raw-bytes fallback could not catch: a
// leading-overlap `%ac` (collapses the 'a' in the decoded view) PLUS an
// in-needle `%5F` (spells the '_' inside the needle, so the raw bytes carry
// "access%5Ftoken=" not the literal "access_token="). Both passes miss; the
// three emitters listed in the bug report would persist the credential
// verbatim. With the percent-tolerant raw matcher, the in-needle escape is
// tolerated and the value is redacted — the matched key form
// (`%access%5Ftoken=`) is preserved verbatim, only the value is rewritten.
func TestWebTabProxyRejectsNonLoopbackTargetRedactsInNeedlePercentEscape(t *testing.T) {
	const secret = "af-sentinel-webtab-loopback-inneedle"
	target := "https://example.com/?%access%5Ftoken=" + secret + "&view=2"
	mux, id, tabID := newWebTabProxyFixture(t, target)

	rec := proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("external in-needle target: status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Errorf("400 body leaked the in-needle token value: %q", body)
	}
	// The matched key form is preserved verbatim (the in-needle `%5F` is kept
	// in the key, only the value is rewritten) per the bug report's recommended
	// key-span choice, so the redaction marker carries the in-needle escape:
	// `access%5Ftoken=REDACTED`, not `access_token=REDACTED`.
	const redactionMarker = "access%5Ftoken=REDACTED"
	if !strings.Contains(body, redactionMarker) {
		t.Errorf("400 body missing the in-needle redaction marker %q; got %q",
			redactionMarker, body)
	}
}

// TestWebTabProxyFailureLogsRedactedInNeedlePercentEscape is the in-needle
// sibling of TestWebTabProxyFailureLogsRedactedOverlapPercentEscape. It
// drives the 502 proxy-failure WarningLog at daemon/webtab_serve.go:538
// (`agentproto.RedactAccessTokenURL(targetURL.String())`) with the same
// combined leading-overlap + in-needle-escape shape. Without the
// percent-tolerant raw matcher the warning log would carry the literal
// `access_token=<value>` substring verbatim. The matched key form is
// preserved (only the value is rewritten).
func TestWebTabProxyFailureLogsRedactedInNeedlePercentEscape(t *testing.T) {
	const secret = "af-sentinel-webtab-proxylog-inneedle"
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	targetURL := dead.URL + "/app?view=2&%access%5Ftoken=" + secret
	dead.Close()

	warnings := captureWarnings(t)

	mux, sessionID, tabID := newWebTabProxyFixture(t, targetURL)
	rec := proxyGet(t, mux, sessionID, tabID, "app?view=2")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if strings.Contains(warnings.String(), secret) {
		t.Fatalf("web-tab proxy log exposed the in-needle token value: %s", warnings.String())
	}
	// The matched key form is preserved verbatim (the in-needle `%5F` is kept
	// in the key, only the value is rewritten) so the redaction marker carries
	// the in-needle escape, not the literal `access_token=REDACTED`.
	const redactionMarker = "access%5Ftoken=REDACTED"
	if !strings.Contains(warnings.String(), redactionMarker) {
		t.Fatalf("web-tab proxy log lost its in-needle redaction marker: %s", warnings.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("web-tab proxy error response exposed the in-needle token value: %s", rec.Body.String())
	}
}
