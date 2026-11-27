package sessionenv

import (
	"os"
	"sort"
)

// caBundleFileCandidates are the platform-default CA bundle FILE locations the
// in-container exec shim re-asserts a path-valued trust pin to, in preference
// order: Debian/Ubuntu/Alpine first, then the RHEL family and its extracted
// trust store, then the BSD/Alpine-alternate spelling. Only the first that
// exists in the container is used — the image's own root bundle is the
// accepted trust class of its binaries, the same residual its /bin/sh is.
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
// trust store instead of neutralizing the name (#7889): OpenSSL treats an
// empty SSL_CERT_FILE/SSL_CERT_DIR as a path that loads zero CAs (measured:
// Python's ssl.create_default_context() goes from ~120 CAs to 0),
// rustls-native-certs reads Some("") and skips the platform roots (codex's
// websocket TLS), and the *CA_BUNDLE/CODEX_CA_CERTIFICATE spellings are the
// same path-valued class for requests, curl, and codex. At the exec shim af
// re-asserts each to the first candidate that EXISTS in the container rather
// than defined-empty, so the name stays claimed — a repo .env loader fills
// only unset names — and points at a real store.
//
// An image with no entry at any candidate keeps the defined-empty pin: on a
// store-less image unset yields the same zero CAs, and defined-empty still
// holds a repo .env claim off. Provisioning exec children get the names
// UNSET instead (TrustPathPinNames) — no .env channel exists there, and
// unset is what makes an env-honoring TLS client fall back to its compiled
// platform store rather than reading the pin's empty path.
//
// Audited path-adjacent pinned names that are NOT here on purpose:
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
var trustPathPinCandidates = map[string][]string{
	"SSL_CERT_FILE":                    caBundleFileCandidates,
	"SSL_CERT_DIR":                     caBundleDirCandidates,
	"NODE_EXTRA_CA_CERTS":              caBundleFileCandidates,
	"REQUESTS_CA_BUNDLE":               caBundleFileCandidates,
	"CURL_CA_BUNDLE":                   caBundleFileCandidates,
	"CODEX_CA_CERTIFICATE":             caBundleFileCandidates,
	"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH": caBundleFileCandidates,
}

// TrustPathPinNames returns the sorted trust-path pin names — the names af's
// in-container provisioning execs unset so an env-honoring TLS client (an
// image git whose libcurl consults the OpenSSL env) uses its compiled
// platform store rather than the pin's defined-empty path.
func TrustPathPinNames() []string {
	out := make([]string, 0, len(trustPathPinCandidates))
	for name := range trustPathPinCandidates {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// trustPathStat is the filesystem probe resolveTrustPathPin consults; tests
// swap it for a temp-dir fixture so resolution is deterministic.
var trustPathStat = os.Stat

// resolveTrustPathPin returns a path-valued pin's defined value for the
// agent's environment: the first candidate that exists in the container
// (directories for SSL_CERT_DIR, files for the bundle names), or defined-and-
// empty when no candidate exists — fail-closed, because an UNSET name is
// claimable by an agent's own .env loader while empty yields the same zero
// CAs on a store-less image. ok reports the name is a path-valued pin.
func resolveTrustPathPin(name string) (entry string, ok bool) {
	candidates, isPathPin := trustPathPinCandidates[name]
	if !isPathPin {
		return "", false
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
		return name + "=" + candidate, true
	}
	return name + "=", true
}
