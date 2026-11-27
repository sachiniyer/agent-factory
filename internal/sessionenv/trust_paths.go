package sessionenv

import (
	"os"
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
// container for a path-valued pin name — a directory for SSL_CERT_DIR, a
// file for SSL_CERT_FILE — or "" when no candidate exists (or the name is
// not a path-valued pin).
func resolveTrustPathValue(name string) string {
	candidates, isPathPin := trustPathPinCandidates[name]
	if !isPathPin {
		return ""
	}
	for _, candidate := range candidates {
		info, err := trustPathStat(candidate)
		if err != nil {
			continue
		}
		if name == "SSL_CERT_DIR" && !info.IsDir() {
			continue
		}
		if name != "SSL_CERT_DIR" && info.IsDir() {
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
// (non-empty) is never touched; SSL_CERT_DIR additionally drops to unset
// rather than resolving whenever SSL_CERT_FILE carries a value, so an
// operator-forwarded bundle file is not silently widened by the image's
// hashed roots (rustls trusts only the file when the dir is unset).
func NeutralizeTrustPathPins() {
	for _, name := range TrustPathPinNames() {
		if value, ok := os.LookupEnv(name); !ok || value != "" {
			continue
		}
		if name == "SSL_CERT_DIR" && os.Getenv("SSL_CERT_FILE") != "" {
			os.Unsetenv(name)
			continue
		}
		if resolved := resolveTrustPathValue(name); resolved != "" {
			os.Setenv(name, resolved)
		} else {
			os.Unsetenv(name)
		}
	}
}
