// Package unixurl owns the one URL form that carries a unix socket path:
// unix:///absolute/path. The provisioner that creates the socket builds it and
// the client that dials it parses it, so both halves live here and cannot drift
// apart (#5870).
//
// The path is percent-escaped when the URL is built and decoded when it is
// parsed. A path is arbitrary bytes the filesystem allows — '#', '?', '%',
// spaces, non-ASCII — and concatenating it raw into a URL makes url.Parse read
// "/srv/af#work/s.sock" as the path "/srv/af" plus a fragment, so the dial goes
// to a socket that does not exist while the carrier forwards the real one.
package unixurl

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Build returns the unix:///… URL naming socket, which must be an absolute path.
func Build(socket string) string {
	u := url.URL{Scheme: "unix", Path: socket}
	return u.String()
}

// Parse recognises a unix:// target. ok is false for any other scheme, which the
// caller parses as http/ws. For a unix target it returns exactly the socket path
// Build was given, or an error when the target is not an absolute, clean path.
func Parse(target string) (socket string, ok bool, err error) {
	u, perr := url.Parse(strings.TrimSpace(target))
	if perr != nil || !strings.EqualFold(u.Scheme, "unix") {
		return "", false, nil
	}
	path := u.Path
	if u.Host != "" || u.Fragment != "" || u.RawQuery != "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", true, fmt.Errorf("invalid daemon url %q: a unix target must be unix:///absolute/clean/path.sock with the path percent-escaped", target)
	}
	return path, true, nil
}
