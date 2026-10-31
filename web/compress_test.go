package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"":                  false,
		"gzip":              true,
		"GZIP":              true,
		"deflate, gzip, br": true,
		"br":                false,
		"identity":          false,
		"gzip;q=0":          false,
		"gzip;q=0.0, br":    false,
		"gzip; q=0.5":       true,
		"*":                 true,
		"*;q=0":             false,
		"br, *;q=0.1":       true,
		"gzip;q=0, *":       false, // the explicit refusal outranks the wildcard
		"gzip;q=banana":     false,
		"compress, x-gzip":  true,
	} {
		if got := AcceptsGzip(header); got != want {
			t.Errorf("AcceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestCompressible(t *testing.T) {
	big := 4096
	for _, c := range []struct {
		name string
		size int
		want bool
	}{
		{"af-web.js", big, true},
		{"af-web.css", big, true},
		{"index.html", big, true},
		{"sw.js", big, true},
		{"manifest.webmanifest", big, true},
		{"icons/icon-192.png", big, false},
		{"icons/mark.svg", big, true},
		{"af-web.js", 100, false}, // too small to be worth a header
		{"AF-WEB.JS", big, true},
	} {
		if got := Compressible(c.name, c.size); got != c.want {
			t.Errorf("Compressible(%q, %d) = %v, want %v", c.name, c.size, got, c.want)
		}
	}
}

func TestGzipCacheRoundTripsAndReuses(t *testing.T) {
	raw := []byte(strings.Repeat("function shell(){return 1}\n", 400))
	var c GzipCache
	first := c.Get("af-web.js", raw)
	if len(first) == 0 || len(first) >= len(raw)/4 {
		t.Fatalf("gzip form is %d bytes for %d raw; want a real reduction", len(first), len(raw))
	}
	zr, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if !bytes.Equal(got, raw) {
		t.Fatal("gzip form does not decompress to the original bytes")
	}
	// Reuse: the second call hands back the same backing bytes, not a recompression.
	second := c.Get("af-web.js", []byte("different input is ignored once cached"))
	if &first[0] != &second[0] {
		t.Fatal("the second Get recompressed instead of reusing the cached form")
	}
}
