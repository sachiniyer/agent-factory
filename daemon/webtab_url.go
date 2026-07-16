package daemon

import (
	"net"
	"net/url"
	"strings"
)

// The URL ALGEBRA of the web-tab mirror: how a proxied tab's paths and queries are
// split, prefixed, and rewritten. Split out of webtab_proxy.go (#1145 file-length
// lint) because it is a distinct, purely FUNCTIONAL layer — no handler, no manager,
// no HTTP — and the proxy file is the one that keeps growing.
//
// One rule runs through all of it, and it is the reason these live together: a URL's
// two views must never be composed side by side. net/url honors RawPath only while
// it unescapes to exactly Path, so the moment the two drift it silently falls back
// to re-encoding Path and the upstream's own escaping is lost — a %2F that is DATA
// inside a segment becomes a separator, naming a different route. Every function
// here therefore derives the pair from ONE string (prefix the escaped form, then
// re-parse) or leaves the raw bytes alone entirely (the query helpers).

// rewriteUpstreamRef maps a URL reference the upstream app emitted into this tab's
// proxy prefix, reporting false when the reference must be passed through
// untouched. It is the shared rule behind Location and Refresh.
//
// Because the browser path MIRRORS the upstream path, the mapping is the same pure
// prefix-prepend that re-scopes cookies:
//
//	/app/                      -> /v1/webtab/s/t/app/          (absolute path: prepend)
//	http://localhost:3000/app/ -> /v1/webtab/s/t/app/          (same upstream: strip origin, keep path)
//	/../login                  -> /v1/webtab/s/t/login         (dot segments resolved first)
//	app/x                      -> unchanged                    (relative: already at mirrored depth)
//	https://example.com/x      -> unchanged                    (foreign host)
//	///example.com/x           -> unchanged                    (foreign host, network-path spelling)
//
// A RELATIVE reference needs no help: the browser resolves it against the current
// proxied URL, which sits at the same depth as the upstream one, so it lands on the
// right path by construction — the same property that makes relative sub-resource
// links work.
//
// A FOREIGN host is passed through verbatim: it is a real off-site redirect (an
// OAuth provider, say) that must leave the frame. Rewriting it would both point the
// browser at a prefix the daemon would then refuse to proxy (only loopback targets
// are proxied) and silently rehost someone else's origin under ours.
//
// Everything here is decided the way the BROWSER will read the header, which is not
// always the way net/url parses it. Two spellings diverge, and both are handled
// before the prefix goes on: a network-path reference net/url hands back as a plain
// path (isNetworkPathRef), and dot segments that would otherwise eat the prefix
// after the browser normalizes them (normalizeEscapedPath).
//
// The path is carried VERBATIM, in the upstream's own encoding: the prefix is
// prepended to the ESCAPED path and the result re-parsed, so url.String() reproduces
// the app's escaping rather than re-canonicalizing it (a literal %2F in a redirect
// target stays %2F, leading or not).
func rewriteUpstreamRef(ref, prefix string, target *url.URL) (string, bool) {
	raw := strings.TrimSpace(ref)
	u, err := url.Parse(raw)
	if err != nil {
		return "", false // unparseable: pass through rather than mangle
	}
	switch {
	case u.Scheme != "" || u.Host != "":
		// Absolute, or protocol-relative (//host/path). Ours to rewrite only if it
		// names the very server we proxy; anything else (including mailto:/data:,
		// which carry no host) leaves untouched.
		if !sameUpstreamHost(u, target) {
			return "", false
		}
		u.Scheme, u.Host, u.User = "", "", nil
	case !strings.HasPrefix(u.Path, "/"):
		return "", false // relative — already mirrored
	case isNetworkPathRef(raw):
		// A foreign host in a spelling net/url reported as a local path. Same rule
		// as any other foreign host: pass it through untouched.
		return "", false
	}
	if u.Path == "" { // origin-only, e.g. http://localhost:3000?x=1
		u.Path = "/"
	}
	// Prefix the escaped form and re-parse it, rather than prefixing Path and RawPath
	// separately: the two must stay consistent or url.String() silently drops RawPath
	// and re-canonicalizes, and a re-parse is what net/url itself uses to keep them so.
	final := strings.TrimRight(prefix, "/") + normalizeEscapedPath(u.EscapedPath())
	if isNetworkPathRef(final) {
		// Unreachable for a real tab prefix (always "/v1/webtab/<sid>/<tab>"), which
		// is exactly why it is asserted rather than assumed: an empty prefix plus an
		// upstream "/..//evil.com" would otherwise emit a Location the browser reads
		// as an off-site host — this proxy handing out an open redirect.
		return "", false
	}
	p, err := url.Parse(final)
	if err != nil {
		return "", false
	}
	u.Path, u.RawPath = p.Path, p.RawPath
	return u.String(), true // query and fragment ride along untouched
}

// isAuthoritySlash reports whether c is a slash the browser's URL parser accepts as
// an authority delimiter for an http(s) URL. Backslash counts: the WHATWG parser
// folds "\" to "/" for these "special" schemes, so "/\host/x" reaches the same
// authority state "//host/x" does.
func isAuthoritySlash(c byte) bool { return c == '/' || c == '\\' }

// isNetworkPathRef reports whether a BROWSER would read ref as naming a HOST rather
// than a path on the current origin — RFC 3986's network-path reference, which the
// browser enters on a leading "//" and, for http(s), on any leading run of slashes
// or backslashes: "///example.com/x" and "/\example.com/x" both navigate to
// example.com.
//
// net/url recognizes only the exact two-slash spelling, filling in Host for it; the
// longer runs it hands back with an EMPTY host and the whole reference as Path. So
// an absolute-path test alone sees a local path and prefixes it, turning an upstream
// "Location: ///accounts.example.com/oauth" into a path on the dev server and
// stranding an OAuth handoff inside the frame.
//
// The test reads the RAW header value because that is the byte string the browser
// parses. A percent-escape is an ordinary path character to it, not a delimiter, so
// "/%2Ffoo" is deliberately NOT a network path here even though net/url decodes its
// Path to "//foo".
func isNetworkPathRef(ref string) bool {
	return len(ref) >= 2 && isAuthoritySlash(ref[0]) && isAuthoritySlash(ref[1])
}

// isSingleDotSegment and isDoubleDotSegment recognize dot segments in the forms a
// BROWSER resolves. The WHATWG URL parser decodes %2e before classifying a segment,
// so "%2e%2e" walks up exactly like ".." does. Neither url.ResolveReference nor
// path.Clean knows that — they compare the literal segment — which is why the rule
// is spelled out here instead of delegated.
func isSingleDotSegment(seg string) bool {
	return seg == "." || strings.EqualFold(seg, "%2e")
}

func isDoubleDotSegment(seg string) bool {
	return seg == ".." || strings.EqualFold(seg, ".%2e") ||
		strings.EqualFold(seg, "%2e.") || strings.EqualFold(seg, "%2e%2e")
}

// normalizeEscapedPath resolves the "." and ".." segments of an absolute escaped
// path, the way the browser resolves them when it follows the rewritten header.
//
// Doing it BEFORE the prefix goes on is what stops a dot segment from eating the
// prefix. An upstream "Location: /../login" prefixed verbatim yields
// "/v1/webtab/<sid>/<tab>/../login"; the browser normalizes that to
// "/v1/webtab/<sid>/login", which names a different tab — or, far more often, a 404
// — instead of the upstream's "/login". Normalizing first sends "/login" through
// the prefix, which is what the upstream meant and what an unproxied browser would
// have fetched.
//
// It walks the ESCAPED path, so a %2F stays an ordinary path character rather than
// becoming a segment separator.
func normalizeEscapedPath(escaped string) string {
	if !strings.HasPrefix(escaped, "/") {
		return escaped
	}
	segments := strings.Split(escaped[1:], "/")
	out := make([]string, 0, len(segments))
	for i, seg := range segments {
		last := i == len(segments)-1
		switch {
		case isSingleDotSegment(seg):
			if last {
				out = append(out, "") // "/a/." names the directory: keep its slash
			}
		case isDoubleDotSegment(seg):
			if len(out) > 0 {
				out = out[:len(out)-1] // at root already: ".." has nothing to pop
			}
			if last {
				out = append(out, "")
			}
		default:
			out = append(out, seg)
		}
	}
	return "/" + strings.Join(out, "/")
}

// sameUpstreamHost reports whether ref names the same origin as the tab's proxied
// target, and so is a self-redirect the proxy should keep inside its prefix.
//
// Loopback ALIASES are deliberately not treated as equal: a target of
// localhost:3000 and a redirect to 127.0.0.1:3000 are the same server in almost
// every setup, but "almost" is what #1810 already paid for here. Binding a frame to
// a server it never named is the failure this file exists to prevent, so an alias
// mismatch degrades to an honest un-rewritten redirect instead. The realistic case
// costs nothing: the Rewrite hook sends the upstream its own Host, so a
// self-redirect echoes the target's host string verbatim.
//
// Scheme must match too. An http->https self-redirect is the app upgrading an origin
// the proxy does not speak; rewriting it would strip the upgrade and hand the
// request straight back to the http upstream, which would redirect again — an
// infinite loop in the frame rather than a visible failure.
func sameUpstreamHost(ref, target *url.URL) bool {
	if ref.Host == "" {
		return false
	}
	scheme := ref.Scheme
	if scheme == "" {
		scheme = target.Scheme // protocol-relative inherits the upstream hop's scheme
	}
	if !strings.EqualFold(scheme, target.Scheme) {
		return false
	}
	return normalizedHostPort(ref.Host, scheme) == normalizedHostPort(target.Host, target.Scheme)
}

// normalizedHostPort renders host:port lowercased with the scheme's default port
// made explicit, so "localhost" and "localhost:80" compare equal under http.
func normalizedHostPort(host, scheme string) string {
	h := strings.ToLower(host)
	if _, _, err := net.SplitHostPort(h); err == nil {
		return h // already carries a port ("[::1]:3000" included)
	}
	switch strings.ToLower(scheme) {
	case "http":
		return h + ":80"
	case "https":
		return h + ":443"
	}
	return h
}

// mirrorRootRedirect computes where a bare request to a tab's proxy root should be
// sent so the browser-visible URL starts MIRRORING the target's path, and reports
// whether a redirect is needed at all.
//
//	prefix /v1/webtab/s/t + target /app/viewer.html -> /v1/webtab/s/t/app/viewer.html, true
//	prefix /v1/webtab/s/t + target /app/            -> /v1/webtab/s/t/app/,            true
//	prefix /v1/webtab/s/t + target /                -> "",                             false
//	prefix /v1/webtab/s/t + target "" (host-only)   -> "",                             false
//
// A root-URL target already mirrors itself, so it is left alone — redirecting it
// to its own path would be an infinite loop. Any other target redirects exactly
// once: the destination's remainder is non-empty, so the follow-up request takes
// the proxy path rather than returning here.
//
// BOTH queries ride along. The incoming one carries the daemon's own
// ?af_webtab_token for a network peer's top-level navigation, so the redirected
// request authorizes even before the browser has stored the token cookie; the
// TARGET's own query is what the tab was pointed at (?doc=123) and dropping it
// would land the app on a different view than the tab names. They address
// different layers, so neither may displace the other.
//
// The path is carried in the target's own escaping, for the reason spelled out on
// escapedRestOf: a %2F inside a segment is data, and re-canonicalizing it here
// would redirect to a route the dev server does not serve.
func mirrorRootRedirect(prefix string, target *url.URL, rawQuery string) (string, bool) {
	if target.Path == "" || target.Path == "/" {
		return "", false
	}
	path, rawPath := joinMirrorPath(prefix, target)
	dest := &url.URL{
		Path:     path,
		RawPath:  rawPath,
		RawQuery: mergeRawQueries(target.RawQuery, rawQuery),
	}
	return dest.String(), true
}

// joinMirrorPath prefixes an upstream URL's path under the tab prefix, returning
// the decoded Path and escaped RawPath as a CONSISTENT pair.
//
// It is rewriteUpstreamRef's technique, on the other entry point: prefix the
// ESCAPED path and RE-PARSE the result, rather than prefixing the decoded and
// escaped views side by side. net/url honors RawPath only while it unescapes to
// exactly Path, so any drift between the two makes it silently fall back to
// re-encoding Path — dropping the raw form the mirror exists to keep. Letting
// net/url derive the pair is what makes them agree by construction.
//
// Prefixing them separately drifts for a path whose FIRST segment carries an
// encoded slash: http://host/%2Ffoo parses to Path "//foo" and EscapedPath
// "/%2Ffoo", and joinURLPath's TrimLeft eats BOTH leading slashes of the decoded
// form but only the one real slash of the escaped one — so Path said ".../foo"
// while RawPath said ".../%2Ffoo", and the redirect flattened the segment.
//
// A path that fails to re-parse keeps the old join rather than dropping the
// redirect: unreachable, since EscapedPath only ever emits valid escapes.
func joinMirrorPath(prefix string, u *url.URL) (path, rawPath string) {
	joined := joinURLPath(prefix, u.EscapedPath())
	p, err := url.Parse(joined)
	if err != nil {
		return joined, joined
	}
	return p.Path, p.RawPath
}

// mergeRawQueries concatenates two raw query strings, dropping empties. Raw
// concatenation rather than parse-and-re-encode, so each side's escaping and
// parameter order survive exactly as written — a dev server that reads its query
// by hand (or signs it) sees what the tab's target actually said.
func mergeRawQueries(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "&" + b
	}
}

// stripRawQueryParam removes every "key=…" (or bare "key") segment from a raw
// query string WITHOUT parsing and re-encoding the rest, so every surviving
// segment keeps its exact bytes and position. url.Values.Encode would sort the
// keys and rewrite escaping (a literal space re-encodes to +), silently changing
// an order- or signature-sensitive query; this touches only the removed key.
//
// THE MATCH IS ON THE DECODED KEY, and that is a security property, not a nicety:
// the STRIPPED set must cover everything the AUTH GATE would ACCEPT. The gate reads
// the token through r.URL.Query(), i.e. url.ParseQuery, which QueryUnescapes key
// NAMES — so "?af%5Fwebtab%5Ftoken=<tok>" authorizes exactly like the plain
// spelling. Matching the raw bytes here accepted that request and then forwarded it
// with the daemon's bearer token STILL IN THE QUERY, handing the credential to the
// previewed dev server: arbitrary user code that has no business holding it, and
// could drive the whole daemon API with it. Decoding each key closes the gap by
// construction, and errs the safe way — it strips anything the gate could read as
// the daemon key, in any spelling.
//
// Unescaping is what makes the two sets agree, so it mirrors ParseQuery exactly: a
// key that fails to unescape is left alone precisely BECAUSE ParseQuery drops that
// segment too, so the gate can never read the token out of it and it is ordinary
// app data.
func stripRawQueryParam(raw, key string) string {
	if raw == "" {
		return ""
	}
	segments := strings.Split(raw, "&")
	kept := make([]string, 0, len(segments))
	for _, seg := range segments {
		name, _, _ := strings.Cut(seg, "=")
		if decoded, err := url.QueryUnescape(name); err == nil && decoded == key {
			continue
		}
		kept = append(kept, seg)
	}
	return strings.Join(kept, "&")
}

// escapedRestOf returns the {rest...} remainder of a proxy request path in the
// request's OWN percent-encoding — the one thing r.PathValue("rest") cannot give.
//
// The mux hands a wildcard back DECODED, so a path that legitimately carries an
// encoded slash — /files/a%2Fb, where %2F is DATA inside ONE segment rather than a
// separator — arrives as "files/a/b", indistinguishable from a real two-segment
// path. Rebuilding the upstream path from that would send the dev server
// /files/a/b: a different route, which lands on a different handler or 404s.
// Forwarding this escaped form keeps a %2F a %2F across the hop — the same
// encoding-preserving property rewriteUpstreamRef already holds for redirects
// coming back the other way.
//
// It SPLITS the escaped path rather than trimming the tab prefix off it, because
// the prefix is assembled from the DECODED sessionId/tabId while this string still
// carries them encoded (the client mints them with encodeURIComponent), so the two
// need not match textually. Splitting is safe for exactly the reason the bug
// exists: in an escaped path a %2F is never a separator, so no id can smuggle one
// in and shift the leading segments.
//
// ok is false only for a path with fewer segments than the route guarantees — not
// reachable through a matched request, but it fails closed rather than assume.
func escapedRestOf(escapedPath string) (string, bool) {
	// "" / "v1" / "webtab" / {sessionId} / {tabId} / {rest...} — one leading empty
	// from the rooted path, the prefix's own segments, then the two ids.
	lead := len(strings.Split(strings.Trim(webtabPathPrefix, "/"), "/")) + 3
	parts := strings.Split(escapedPath, "/")
	if len(parts) < lead {
		return "", false
	}
	return strings.Join(parts[lead:], "/"), true
}

// joinURLPath joins a base path and a sub path with exactly one slash between
// them. Used to re-scope an upstream cookie's Path under the tab's proxy prefix.
// Because the browser path mirrors the upstream path, prepending the prefix is all
// a correct re-scope takes.
func joinURLPath(base, sub string) string {
	if base == "" || base == "/" {
		return sub
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(sub, "/")
}
