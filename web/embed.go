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
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sync"
)

//go:embed dist
var distFS embed.FS

// Dist returns the embedded built web assets rooted at dist/, so "index.html"
// and "af-web.js" are top-level names. The dist/ tree is committed, so the
// fs.Sub can only fail on a build-time embed bug (a missing dist/ directory),
// which is a programmer error worth panicking on rather than threading an error
// through every serving call site.
//
// Every file is served as embedded except sw.js, whose cache name is stamped here;
// see stampServiceWorker.
func Dist() fs.FS {
	return stampedDist()
}

// stampedDist builds Dist's FS once. Its inputs are fixed when the binary is
// compiled, so a failure here fails every call of every build of the same tree —
// including this package's tests — and can never first appear on a user's machine.
var stampedDist = sync.OnceValue(func() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("web: embedded dist/ missing (run `make web-build`): " + err.Error())
	}
	stamped, err := stampServiceWorker(sub)
	if err != nil {
		panic("web: " + err.Error() + " (run `make web-build`)")
	}
	return stamped
})

const (
	// serviceWorkerName is the one embedded file Dist does not serve verbatim.
	serviceWorkerName = "sw.js"
	// shellVersionPlaceholder is what src/sw.js names its cache with. build.mjs
	// copies the worker without replacing it and refuses to build without it.
	shellVersionPlaceholder = "__AF_SHELL_VERSION__"
)

// shellFiles are the files whose bytes name the worker's cache. They are hashed in
// this order, the order build.mjs used when it stamped the committed worker, so a
// binary serving an unchanged shell names the same cache as one built before the
// stamp moved here and an upgrade across that change does not discard it.
var shellFiles = []string{"af-web.js", "af-web.css", "index.html"}

// stampServiceWorker returns fsys with the placeholder in sw.js replaced by
// shellVersion (#4116).
//
// The stamp lives here, not in the committed file, because of what it is: a hash of
// the whole shell. When two branches both change the shell, the right value for
// their merge is the hash of the merged shell, which neither branch committed. With
// the stamp committed, every pair of open web PRs conflicted on that one line, and
// taking either side mechanically would have committed a stale name. Computed from
// the embedded bytes, it is right for whatever the binary serves, and the committed
// dist/sw.js only changes when src/sw.js does.
func stampServiceWorker(fsys fs.FS) (fs.FS, error) {
	version, err := shellVersion(fsys)
	if err != nil {
		return nil, err
	}
	raw, err := fs.ReadFile(fsys, serviceWorkerName)
	if err != nil {
		return nil, fmt.Errorf("read embedded %s: %w", serviceWorkerName, err)
	}
	info, err := fs.Stat(fsys, serviceWorkerName)
	if err != nil {
		return nil, fmt.Errorf("stat embedded %s: %w", serviceWorkerName, err)
	}
	// Every occurrence: the browser must never see the placeholder. A worker with none
	// is served as committed; TestCommittedServiceWorkerIsUnstamped is what refuses
	// that tree, since stopping the daemon over it would cost far more than a cache.
	stamped := bytes.ReplaceAll(raw, []byte(shellVersionPlaceholder), []byte(version))
	return overlayFS{
		FS:   fsys,
		name: serviceWorkerName,
		data: stamped,
		info: sizedFileInfo{FileInfo: info, size: int64(len(stamped))},
	}, nil
}

// shellVersion is the first 12 hex digits of the SHA-256 of the shell files'
// concatenated bytes. It is a CONTENT hash rather than the af version on purpose: CI
// bumps main.go's version without rebuilding web/dist, so a version stamp would name
// a cache for a build it was never part of.
func shellVersion(fsys fs.FS) (string, error) {
	h := sha256.New()
	for _, name := range shellFiles {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return "", fmt.Errorf("read embedded shell file %s: %w", name, err)
		}
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// overlayFS serves data in place of one file and delegates every other name. Only
// Open is promoted from the embedded fs.FS, so ReadFile, Stat and Sub all reach the
// replacement through it rather than around it. Directory listings still come from
// the underlying tree and report the file's unstamped size; nothing lists this one.
type overlayFS struct {
	fs.FS
	name string
	data []byte
	info fs.FileInfo
}

func (o overlayFS) Open(name string) (fs.File, error) {
	if name != o.name {
		return o.FS.Open(name)
	}
	return &overlayFile{Reader: bytes.NewReader(o.data), info: o.info}, nil
}

// overlayFile is an in-memory fs.File. The embedded *bytes.Reader also makes it an
// io.Seeker, which http.FileServer needs to answer a Range request.
type overlayFile struct {
	*bytes.Reader
	info fs.FileInfo
}

func (f *overlayFile) Stat() (fs.FileInfo, error) { return f.info, nil }

func (f *overlayFile) Close() error { return nil }

// sizedFileInfo is the embedded file's FileInfo with the stamped length.
type sizedFileInfo struct {
	fs.FileInfo
	size int64
}

func (i sizedFileInfo) Size() int64 { return i.size }
