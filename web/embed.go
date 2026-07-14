// Package web embeds the built browser web client (#1592 Phase 5) into the af
// binary so the daemon can serve a self-contained SPA over its HTTP TCP listener
// with no external assets and no Node toolchain at `go build` time.
//
// The committed web/dist/ tree (the esbuild bundle + the index.html shell +
// the extracted CSS, produced by `make web-build`, design §1.3/§3.3) is the sole
// embed root. Keeping dist/ committed means `go build ./...` and the Go test
// suite never need Node — the JS toolchain is gated entirely behind `make web-*`.
//
// This package is a pure leaf: it embeds assets and exposes them as an fs.FS. All
// serving, routing, and the auth/CSP policy live in the daemon (daemon/webserve.go),
// which composes this FS behind the same token gate the API rides.
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var distFS embed.FS

// Dist returns the embedded built web assets rooted at dist/, so "index.html"
// and "af-web.js" are top-level names. The dist/ tree is committed, so the
// fs.Sub can only fail on a build-time embed bug (a missing dist/ directory),
// which is a programmer error worth panicking on rather than threading an error
// through every serving call site.
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("web: embedded dist/ missing (run `make web-build`): " + err.Error())
	}
	return sub
}
