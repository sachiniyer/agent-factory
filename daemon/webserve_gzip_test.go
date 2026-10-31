package daemon

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// getEncoded fetches path with exactly the given Accept-Encoding (none when empty).
// DisableCompression stops Go's transport from adding and undoing gzip itself,
// so the test sees the bytes and headers a browser would.
func getEncoded(t *testing.T, base, method, path, acceptEncoding string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, base+path, nil)
	require.NoError(t, err)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, body
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	return out
}

// #6417: the shell is ~1 MB raw; clients that accept gzip get the compressed form,
// everyone else the identical raw bytes, and Vary says which is which.
func TestServeSPA_GzipWhenAccepted(t *testing.T) {
	srv := httptest.NewServer(webShellHandler(&apiSpy{}))
	defer srv.Close()

	rawResp, raw := getEncoded(t, srv.URL, http.MethodGet, "/af-web.js", "")
	require.Equal(t, http.StatusOK, rawResp.StatusCode)
	require.Empty(t, rawResp.Header.Get("Content-Encoding"), "a client that did not ask must get identity")
	require.Contains(t, rawResp.Header.Get("Vary"), "Accept-Encoding", "even the identity answer must vary, or a shared cache poisons gzip clients")
	require.Greater(t, len(raw), 100_000)

	gzResp, gz := getEncoded(t, srv.URL, http.MethodGet, "/af-web.js", "gzip, deflate, br")
	require.Equal(t, http.StatusOK, gzResp.StatusCode)
	require.Equal(t, "gzip", gzResp.Header.Get("Content-Encoding"))
	require.Contains(t, gzResp.Header.Get("Vary"), "Accept-Encoding")
	require.Contains(t, gzResp.Header.Get("Content-Type"), "javascript")
	require.Equal(t, "nosniff", gzResp.Header.Get("X-Content-Type-Options"))
	require.NotEmpty(t, gzResp.Header.Get("Content-Security-Policy"))
	require.Equal(t, strconv.Itoa(len(gz)), gzResp.Header.Get("Content-Length"))
	require.Less(t, len(gz), len(raw)/2, "the compressed shell must be much smaller than the raw one")
	require.Equal(t, raw, gunzip(t, gz), "gzip must decode to exactly the identity bytes")

	// An explicit refusal (or no mention) of gzip gets identity.
	for _, ae := range []string{"gzip;q=0", "br, deflate", "identity"} {
		resp, body := getEncoded(t, srv.URL, http.MethodGet, "/af-web.js", ae)
		require.Empty(t, resp.Header.Get("Content-Encoding"), ae)
		require.Equal(t, raw, body, ae)
	}

	// HEAD announces the encoding without a body.
	head, hb := getEncoded(t, srv.URL, http.MethodHead, "/af-web.js", "gzip")
	require.Equal(t, "gzip", head.Header.Get("Content-Encoding"))
	require.Empty(t, hb)
}

// The service worker is stamped at runtime (stampServiceWorker); its compressed
// form must carry the stamp, not the build placeholder, and the SPA fallback is
// compressed like the shell it serves.
func TestServeSPA_GzipKeepsStampAndFallback(t *testing.T) {
	srv := httptest.NewServer(webShellHandler(&apiSpy{}))
	defer srv.Close()

	resp, body := getEncoded(t, srv.URL, http.MethodGet, "/sw.js", "gzip")
	if resp.Header.Get("Content-Encoding") == "gzip" {
		body = gunzip(t, body)
	}
	require.NotContains(t, string(body), "__AF_SHELL_VERSION__", "the stamped service worker must be served, compressed or not")

	idx, _ := getEncoded(t, srv.URL, http.MethodGet, "/", "gzip")
	deep, deepBody := getEncoded(t, srv.URL, http.MethodGet, "/some/deep/link", "gzip")
	require.Equal(t, idx.StatusCode, deep.StatusCode)
	require.Equal(t, idx.Header.Get("Content-Encoding"), deep.Header.Get("Content-Encoding"))
	if deep.Header.Get("Content-Encoding") == "gzip" {
		deepBody = gunzip(t, deepBody)
	}
	require.True(t, strings.Contains(string(deepBody), "<html") || strings.Contains(strings.ToLower(string(deepBody)), "<!doctype"))
}
