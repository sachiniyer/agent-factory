package web

import (
	"bytes"
	"compress/gzip"
	"path"
	"strconv"
	"strings"
	"sync"
)

// The shell is a ~1 MB script and ~100 KB of CSS. The perf budgets in
// docs/dev/perf-baselines.md are gzip budgets (~256 KB), so serving the raw bytes
// put 4x the budgeted weight on the wire — 20+ s of "Connecting…" on a 3G link
// (#6417). The daemon serves the compressed form when the request allows it.
//
// Compression happens once per process per file, on first request, and the result
// is reused. It is not done at build time because the committed web/dist is checked
// byte-for-byte for reproducibility (compressor output differs across Node/zlib
// versions) and because sw.js is stamped here at runtime (see stampServiceWorker),
// so a build-time sw.js.gz would carry the unstamped placeholder.

// minCompressSize is the size below which compressing a file saves less than the
// header it costs.
const minCompressSize = 1024

// compressibleExt are the text formats worth compressing. Images and fonts are
// already compressed and are served as embedded.
var compressibleExt = map[string]bool{
	".js": true, ".css": true, ".html": true, ".svg": true,
	".json": true, ".webmanifest": true, ".txt": true,
}

// Compressible reports whether the named embedded file of the given size is served
// in a compressed form to clients that accept one. It also decides which responses
// must carry "Vary: Accept-Encoding": every compressible file does, whichever form
// a given client gets.
func Compressible(name string, size int) bool {
	return size >= minCompressSize && compressibleExt[strings.ToLower(path.Ext(name))]
}

// AcceptsGzip reports whether an Accept-Encoding header value admits gzip: it
// names gzip (or "*") with a non-zero quality.
func AcceptsGzip(header string) bool {
	star := false
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		coding := strings.ToLower(strings.TrimSpace(fields[0]))
		if coding != "gzip" && coding != "x-gzip" && coding != "*" {
			continue
		}
		q := 1.0
		for _, p := range fields[1:] {
			p = strings.TrimSpace(p)
			if v, ok := strings.CutPrefix(strings.ToLower(p), "q="); ok {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = parsed
				} else {
					q = 0
				}
			}
		}
		if coding == "*" {
			star = q > 0
			continue
		}
		// An explicit gzip entry decides it, even if a "*" appears too.
		return q > 0
	}
	return star
}

// GzipCache holds the gzip form of each file it has been asked for. The zero value
// is ready to use. It is keyed by name alone: one cache serves one fixed set of
// embedded files, which cannot change while the process runs.
type GzipCache struct {
	mu sync.Mutex
	m  map[string][]byte
}

// Get returns the gzip encoding of raw, the content of the embedded file name,
// compressing it on the first call for that name (level 9: it runs once).
func (c *GzipCache) Get(name string, raw []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gz, ok := c.m[name]; ok {
		return gz
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	if c.m == nil {
		c.m = make(map[string][]byte)
	}
	c.m[name] = buf.Bytes()
	return c.m[name]
}
