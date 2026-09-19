package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

// stampedVersionLine matches a worker whose cache name was filled in with a content
// hash, which is what build.mjs used to commit.
var stampedVersionLine = regexp.MustCompile(`const VERSION = "[0-9a-f]{12}"`)

// TestCommittedServiceWorkerIsUnstamped is #4116. A stamp in the COMMITTED worker is
// a line whose correct value is a hash of the whole merged shell, so two branches that
// both change the shell always disagree on it, and neither side's value is right for
// the merge. The committed file must carry the placeholder; Dist fills it in.
func TestCommittedServiceWorkerIsUnstamped(t *testing.T) {
	raw, err := fs.ReadFile(distFS, "dist/sw.js")
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(raw), shellVersionPlaceholderForTest),
		"dist/sw.js must carry the %s placeholder exactly once — run `make web-build`", shellVersionPlaceholderForTest)
	require.NotRegexp(t, stampedVersionLine, string(raw),
		"dist/sw.js must not commit a stamped cache name; the stamp is applied when the file is served")
}

// TestDistServesTheWorkerStampedWithTheShellHash pins what the browser receives:
// the placeholder replaced by a hash of the shell the worker caches. The hash is
// recomputed here from the raw embedded bytes rather than through the package, so
// dropping a file from the set, or reordering it, fails this test.
func TestDistServesTheWorkerStampedWithTheShellHash(t *testing.T) {
	h := sha256.New()
	for _, name := range []string{"af-web.js", "af-web.css", "index.html"} {
		data, err := fs.ReadFile(distFS, "dist/"+name)
		require.NoError(t, err)
		h.Write(data)
	}
	want := hex.EncodeToString(h.Sum(nil))[:12]

	body, err := fs.ReadFile(Dist(), "sw.js")
	require.NoError(t, err)
	// Booleans rather than Contains, which would print the whole worker on failure.
	require.False(t, strings.Contains(string(body), shellVersionPlaceholderForTest),
		"the served worker must never name the literal placeholder cache")
	require.True(t, strings.Contains(string(body), `const VERSION = "`+want+`";`),
		"the served worker must name the cache af-shell-%s", want)
	t.Logf("served cache name: af-shell-%s", want)

	// A reader that sizes its buffer from Stat must see the stamped length.
	info, err := fs.Stat(Dist(), "sw.js")
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), info.Size())
	require.False(t, info.IsDir())
	require.Equal(t, "sw.js", info.Name())
}

// TestDistServesEveryOtherAssetVerbatim keeps the stamp from reaching anything but
// the worker.
func TestDistServesEveryOtherAssetVerbatim(t *testing.T) {
	served := Dist()
	count := 0
	err := fs.WalkDir(distFS, "dist", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || name == "dist/sw.js" {
			return err
		}
		want, err := fs.ReadFile(distFS, name)
		require.NoError(t, err)
		got, err := fs.ReadFile(served, strings.TrimPrefix(name, "dist/"))
		require.NoError(t, err)
		require.Equal(t, want, got, "%s must be served exactly as embedded", name)
		count++
		return nil
	})
	require.NoError(t, err)
	require.Greater(t, count, 3, "the walk must actually cover the embedded assets")
}

// TestShellVersionFollowsOnlyTheShellBytes pins which bytes name the cache: a change to
// any shell file must move it, and a change to anything else must not, or an upgrade
// would discard a cache that still holds exactly the shell it serves.
func TestShellVersionFollowsOnlyTheShellBytes(t *testing.T) {
	tree := func() fstest.MapFS {
		return fstest.MapFS{
			"af-web.js":      {Data: []byte("bundle")},
			"af-web.css":     {Data: []byte("styles")},
			"index.html":     {Data: []byte("<html>")},
			"sw.js":          {Data: []byte(`const VERSION = "__AF_SHELL_VERSION__";`)},
			"icons/icon.svg": {Data: []byte("<svg>")},
		}
	}
	version := func(t *testing.T, fsys fs.FS) string {
		t.Helper()
		v, err := shellVersion(fsys)
		require.NoError(t, err)
		require.Regexp(t, `^[0-9a-f]{12}$`, v)
		return v
	}
	base := version(t, tree())
	require.Equal(t, base, version(t, tree()), "the same shell must always name the same cache")

	for _, name := range []string{"af-web.js", "af-web.css", "index.html"} {
		changed := tree()
		changed[name] = &fstest.MapFile{Data: append([]byte("changed "), changed[name].Data...)}
		require.NotEqual(t, base, version(t, changed), "changing %s must name a new cache", name)
	}
	for _, name := range []string{"sw.js", "icons/icon.svg"} {
		changed := tree()
		changed[name] = &fstest.MapFile{Data: []byte("changed")}
		require.Equal(t, base, version(t, changed), "changing %s must keep the cache name", name)
	}

	missing := tree()
	delete(missing, "af-web.css")
	_, err := shellVersion(missing)
	require.ErrorContains(t, err, "af-web.css")
}

// TestStampServiceWorkerReplacesEveryPlaceholder: the browser must never see the
// literal, however many times the worker names it.
func TestStampServiceWorkerReplacesEveryPlaceholder(t *testing.T) {
	tree := fstest.MapFS{
		"af-web.js":  {Data: []byte("bundle")},
		"af-web.css": {Data: []byte("styles")},
		"index.html": {Data: []byte("<html>")},
		"sw.js":      {Data: []byte("a __AF_SHELL_VERSION__ b __AF_SHELL_VERSION__ c")},
	}
	want, err := shellVersion(tree)
	require.NoError(t, err)
	stamped, err := stampServiceWorker(tree)
	require.NoError(t, err)
	body, err := fs.ReadFile(stamped, "sw.js")
	require.NoError(t, err)
	require.Equal(t, "a "+want+" b "+want+" c", string(body))

	// A worker with no placeholder is served as committed, not refused at startup.
	tree["sw.js"] = &fstest.MapFile{Data: []byte("no placeholder")}
	stamped, err = stampServiceWorker(tree)
	require.NoError(t, err)
	body, err = fs.ReadFile(stamped, "sw.js")
	require.NoError(t, err)
	require.Equal(t, "no placeholder", string(body))
}

// TestDistWorksBehindHTTPFileServer covers the other common way to serve an fs.FS,
// which needs the replacement file to report its real length, and to seek for a Range
// request.
func TestDistWorksBehindHTTPFileServer(t *testing.T) {
	want, err := fs.ReadFile(Dist(), "sw.js")
	require.NoError(t, err)

	srv := httptest.NewServer(http.FileServer(http.FS(Dist())))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/sw.js")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int64(len(want)), resp.ContentLength)
	require.True(t, string(got) == string(want), "http.FileServer must serve the stamped worker")

	// A Range request is the one that seeks.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/sw.js", nil)
	require.NoError(t, err)
	req.Header.Set("Range", "bytes=10-19")
	ranged, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = ranged.Body.Close() }()
	part, err := io.ReadAll(ranged.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, ranged.StatusCode)
	require.Equal(t, string(want[10:20]), string(part))
}

// shellVersionPlaceholderForTest is spelled out rather than taken from the package so
// a rename on only one side of the build.mjs/Go boundary fails here.
const shellVersionPlaceholderForTest = "__AF_SHELL_VERSION__"
