package sessionenv

import (
	"os"
	"slices"
	"sort"
)

// caBundleFileCandidates are the platform-default CA bundle FILE locations the
// in-container exec shim re-asserts SSL_CERT_FILE to, in preference order:
// Debian/Ubuntu/Alpine first, then the RHEL family and its extracted trust
// store, then the BSD/Alpine-alternate spelling. Only the first that exists in
// the container is used — the image's own root bundle is the accepted trust
// class of its binaries, the same residual its /bin/sh is.
var caBundleFileCandidates = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// caBundleDirCandidates are the hashed trust-DIRECTORY locations for
// SSL_CERT_DIR — the one trust name whose value is a directory.
var caBundleDirCandidates = []string{
	"/etc/ssl/certs",
	"/etc/pki/tls/certs",
}

// trustPathPinCandidates names the credential-mount env pins whose
// defined-empty value a TLS stack reads as a PATH — where empty empties the
// trust store instead of neutralizing the name (#7889). The set is the two
// names the independent review MEASURED reading empty as a path:
//
//   - SSL_CERT_FILE: OpenSSL treats the empty value as a path that loads zero
//     CAs (measured: Python 3.12 ssl.create_default_context cert_store_stats
//     goes from ~120 CAs to 0), and rustls-native-certs reads Some("") and
//     skips the platform roots (codex's websocket TLS).
//   - SSL_CERT_DIR: rustls-native-certs reads Some("") the same way. (OpenSSL
//     alone tolerates the empty dir — 120 CAs — but FILE="" beside a resolved
//     DIR still gives 0, so the pair resolves together.)
//
// At the exec shim af re-asserts each to the first candidate that EXISTS in
// the container rather than defined-empty, so the name stays claimed — a
// repo .env loader fills only unset names — and points at a real store. The
// agent-server additionally resolves-or-unsets them in its own environment
// at startup (NeutralizeTrustPathPins) so every child it spawns —
// post-worktree hooks included — inherits a real store or an absent name.
//
// An image with no entry at any candidate keeps the defined-empty pin: on a
// store-less image unset yields the same zero CAs, and defined-empty still
// holds a repo .env claim off. Provisioning exec children get the names
// UNSET instead (TrustPathPinNames) — no .env channel exists there, and
// unset is what makes an env-honoring TLS client fall back to its compiled
// platform store rather than reading the pin's empty path.
//
// Audited path-adjacent pinned names that are deliberately NOT here —
// measured reads showed the empty value is falsy-safe for each:
//   - NODE_EXTRA_CA_CERTS: node ignores the empty value.
//   - REQUESTS_CA_BUNDLE: requests falls through to CURL_CA_BUNDLE.
//   - CURL_CA_BUNDLE: curl uses its compiled CAfile.
//   - CODEX_CA_CERTIFICATE: codex's custom_ca.rs treats empty as unset.
//   - GRPC_DEFAULT_SSL_ROOTS_FILE_PATH: grpc-js skips the empty value (the
//     repo's own audit at gemini_bundle_env.go).
//   - GH_CONFIG_DIR, GNUPGHOME, XDG_RUNTIME_DIR, CODEX_SQLITE_HOME: their
//     readers fall back to a default on empty (the measured empty→default
//     behavior recorded for credentialMountSharedConfigRootNames).
//   - GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM: re-asserted to real roots
//     already (credentialMountReassertedNames) — git treats defined-empty
//     as a real path and fails, which is why they are not pinned.
//   - GIT_CONFIG_COUNT/GIT_CONFIG_PARAMETERS: empty is the desired
//     semantics (git reads count 0 / no -c pairs).
//   - GOOGLE_API_CERTIFICATE_CONFIG: google-auth-library gates the read on
//     isValidFile, so empty is skipped.
//   - DBUS_SESSION_BUS_ADDRESS / proxies / GH_HOST / AF_DAEMON_URL /
//     AF_DAEMON_TOKEN: not filesystem paths — empty reads as unset for
//     their readers.
//
// Resolving the falsy-safe names would also be wrong, not merely harmless:
// a resolved platform bundle OUTRANKS an operator-forwarded sibling in the
// reader chains that consult more than one name — curl prefers
// CURL_CA_BUNDLE over SSL_CERT_FILE for --cacert, Requests prefers
// REQUESTS_CA_BUNDLE over CURL_CA_BUNDLE, and grpc-js's
// GRPC_DEFAULT_SSL_ROOTS_FILE_PATH replaces node's roots where
// NODE_EXTRA_CA_CERTS was meant to add them. Defined-empty leaves every
// chain's operator-forwarded member as the first non-empty read.
var trustPathPinCandidates = map[string][]string{
	"SSL_CERT_FILE": caBundleFileCandidates,
	"SSL_CERT_DIR":  caBundleDirCandidates,
	// git's own CA-root spellings map to the same curl slots: CAINFO sets
	// CAfile, CAPATH sets the hashed directory — and the measured empty
	// read is a broken path ("Problem with the SSL CA cert"), the same
	// class as SSL_CERT_FILE (#7891). GIT_PROXY_SSL_* are their
	// proxy-connection twins.
	"GIT_SSL_CAINFO":       caBundleFileCandidates,
	"GIT_SSL_CAPATH":       caBundleDirCandidates,
	"GIT_PROXY_SSL_CAINFO": caBundleFileCandidates,
	"GIT_PROXY_SSL_CAPATH": caBundleDirCandidates,
}

// trustPathSiblingPairs maps a path-valued trust name to the sibling whose
// forwarded claim keeps the pin defined-empty — only where the empty value
// truly contributes nothing beside its sibling. SSL_CERT_FILE/SSL_CERT_DIR
// are independent slots every reader loads, so beside a non-empty
// (forwarded) member the pinned member must keep its defined-empty claim
// rather than resolving: the resolved slot would silently widen the
// operator's trust with the image's roots, while defined-empty contributes
// none (#7889 R5 + mirror). git's CA pair is ASYMMETRIC (measured on git
// 2.43): an empty CAPATH beside a valid CAINFO contributes nothing — the
// guard stands — but an empty CAINFO poisons even a valid CAPATH
// ("Problem with the SSL CA cert"), so CAINFO has no safe defined value
// beside a forwarded sibling and is resolved/unset normally instead.
var trustPathSiblingPairs = map[string]string{
	"SSL_CERT_FILE":        "SSL_CERT_DIR",
	"SSL_CERT_DIR":         "SSL_CERT_FILE",
	"GIT_SSL_CAPATH":       "GIT_SSL_CAINFO",
	"GIT_PROXY_SSL_CAPATH": "GIT_PROXY_SSL_CAINFO",
}

// trustPresenceNames are the trust-relevant env names with NO inert defined
// value: git reads GIT_SSL_NO_VERIFY by getenv presence, so ANY defined
// value — `=` and `=0` included, measured on git 2.43 — turns verification
// off; defined-empty is the attack rather than the pin. GIT_SSL_CERT is the
// converse: `=` makes curl eagerly fail loading a client cert, and no
// platform default exists to resolve to; GIT_SSL_KEY and
// GIT_SSL_CERT_PASSWORD only modulate a set CERT, and
// GIT_HTTP_PROXY_AUTHMETHOD selects a proxy-auth scheme empty does not
// name. These names are never `-e NAME=`'d; af strips them at the exec
// shim, at agent-server startup, and in provisioning exec preambles —
// unset is the only spelling that contributes nothing (#7891). An
// operator-forwarded name stays verbatim.
var trustPresenceNames = map[string]struct{}{
	"GIT_SSL_NO_VERIFY":         {},
	"GIT_PROXY_SSL_NO_VERIFY":   {},
	"GIT_SSL_CERT":              {},
	"GIT_SSL_KEY":               {},
	"GIT_SSL_CERT_PASSWORD":     {},
	"GIT_HTTP_PROXY_AUTHMETHOD": {},
}

// TrustPresenceStripNames returns the sorted names af strips (rather than
// pins) at af-controlled env boundaries — the trust names no defined value
// can render inert.
func TrustPresenceStripNames() []string {
	out := make([]string, 0, len(trustPresenceNames))
	for name := range trustPresenceNames {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TrustPresenceName reports whether name is one af strips rather than pins.
func TrustPresenceName(name string) bool {
	_, ok := trustPresenceNames[name]
	return ok
}

// TrustPathSibling returns the sibling name whose forwarded claim keeps a
// path pin defined-empty, and whether the pair applies.
func TrustPathSibling(name string) (string, bool) {
	sibling, ok := trustPathSiblingPairs[name]
	return sibling, ok
}

// TrustPathPinNames returns the sorted trust-path pin names — the names af's
// in-container provisioning execs unset so every process af starts sees them
// unset-or-resolved, never defined-empty.
func TrustPathPinNames() []string {
	out := make([]string, 0, len(trustPathPinCandidates))
	for name := range trustPathPinCandidates {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// trustPathStat is the filesystem probe resolveTrustPathValue consults; tests
// swap it for a temp-dir fixture so resolution is deterministic.
var trustPathStat = os.Stat

// resolveTrustPathValue returns the first candidate that exists in the
// container for a path-valued pin name — a directory for the CAPATH names,
// a file for the rest — or "" when no candidate exists (or the name is
// not a path-valued pin).
func resolveTrustPathValue(name string) string {
	candidates, isPathPin := trustPathPinCandidates[name]
	if !isPathPin {
		return ""
	}
	dirWanted := slices.Equal(candidates, caBundleDirCandidates)
	for _, candidate := range candidates {
		info, err := trustPathStat(candidate)
		if err != nil || info.IsDir() != dirWanted {
			continue
		}
		return candidate
	}
	return ""
}

// resolveTrustPathPin returns a path-valued pin's defined value for the
// agent's environment: the first candidate that exists in the container, or
// defined-and-empty when no candidate exists — fail-closed, because an UNSET
// name is claimable by an agent's own .env loader while empty yields the
// same zero CAs on a store-less image. ok reports the name is a path-valued
// pin.
func resolveTrustPathPin(name string) (entry string, ok bool) {
	if _, isPathPin := trustPathPinCandidates[name]; !isPathPin {
		return "", false
	}
	if resolved := resolveTrustPathValue(name); resolved != "" {
		return name + "=" + resolved, true
	}
	return name + "=", true
}

// NeutralizeTrustPathPins re-points each path-valued trust name that stands
// defined-and-empty in THIS process's environment — the agent-server runs it
// once at startup so every non-pane child it spawns (post-worktree hooks,
// helpers) inherits a real store or an absent name, never the pin's empty
// path (#7889). The container env carries `NAME=` to dominate an image ENV;
// here, at the last process af controls before spawning children, the empty
// value resolves to a platform bundle that exists, or drops to unset when
// the image has no store at all — no .env-fill channel reaches these
// children, so unset is the inert form. A name the operator forwarded
// (non-empty) is never touched.
//
// The exception is a claimed sibling: the names in trustPathSiblingPairs
// are independent slots every reader loads, so beside a non-empty
// (forwarded) sibling the empty slot contributes NOTHING to the operator's
// trust — and defined-empty is the only spelling that says so: UNSET would
// restore the platform default for OpenSSL and openssl-probe alike, and
// resolved would add the image's roots outright (#7889 R5 + its mirror).
// The empty slot keeps its defined-empty shape then, deliberately.
//
// forwarded is the operator's session_env_passthrough claim list: a
// presence-read name (trustPresenceNames) the operator claimed keeps its
// value verbatim; any other defined instance is unset — no defined value
// is inert for those names (#7891).
func NeutralizeTrustPathPins(forwarded []string) {
	for _, name := range TrustPresenceStripNames() {
		if slices.Contains(forwarded, name) {
			continue
		}
		if _, ok := os.LookupEnv(name); ok {
			os.Unsetenv(name)
		}
	}
	// Snapshot sibling claims before the loop — resolving one member would
	// otherwise read as a claim and guard the other, where the operator's
	// forwarded (pre-existing non-empty) value is what the guard protects.
	siblingClaimed := make(map[string]bool, len(trustPathSiblingPairs))
	for _, sibling := range trustPathSiblingPairs {
		siblingClaimed[sibling] = os.Getenv(sibling) != ""
	}
	for _, name := range TrustPathPinNames() {
		if value, ok := os.LookupEnv(name); !ok || value != "" {
			continue
		}
		if sibling, paired := trustPathSiblingPairs[name]; paired && siblingClaimed[sibling] {
			continue
		}
		if resolved := resolveTrustPathValue(name); resolved != "" {
			os.Setenv(name, resolved)
		} else {
			os.Unsetenv(name)
		}
	}
}
