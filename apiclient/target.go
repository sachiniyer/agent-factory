package apiclient

import "os"

// Remote-target resolution (#1592 Phase 3 PR4, §1.6). A TUI/CLI points at a
// remote daemon with flags, falling back to env; unset ⇒ the local unix socket
// (today's behavior, unchanged). The precedence is flag > env, resolved lazily at
// each client-construction site so a flag parsed by the root command and an env
// var in a bare `af sessions ...` invocation both take effect through one path.
//
// There is deliberately no client-side config FILE (§1.6): flags/env suffice for
// a single-owner operator, and a client can hold at most one daemon target, so a
// per-repo/global config layer would only add ambiguity. A `known_daemons` pin
// file is an additive follow-up if managing several remotes by hand becomes
// annoying — explicitly out of scope here.

// Env-var names for the remote target, the fallback when the matching flag is
// unset. Named on the AF_DAEMON_* namespace so they read as "which daemon".
const (
	envDaemonURL      = "AF_DAEMON_URL"
	envDaemonToken    = "AF_DAEMON_TOKEN"
	envTLSFingerprint = "AF_DAEMON_TLS_FINGERPRINT"
)

// Flag-backed remote-target values, bound by the root command's persistent flags
// (commands/root.go). Empty ⇒ fall back to the matching env var. These are the
// ONLY package-level mutable state the target resolver reads; a test sets them
// directly (and restores them) to exercise the remote path without cobra.
var (
	FlagDaemonURL      string
	FlagDaemonToken    string
	FlagTLSFingerprint string
)

// resolveTarget merges flag > env into the effective remote target. An empty
// daemonURL means "no remote target" — the caller uses the local unix socket.
func resolveTarget() (daemonURL, token, fingerprint string) {
	daemonURL = firstNonEmpty(FlagDaemonURL, os.Getenv(envDaemonURL))
	token = firstNonEmpty(FlagDaemonToken, os.Getenv(envDaemonToken))
	fingerprint = firstNonEmpty(FlagTLSFingerprint, os.Getenv(envTLSFingerprint))
	return
}

// IsRemoteTarget reports whether a remote daemon target is configured (a non-empty
// --daemon-url / AF_DAEMON_URL). Callers use it to skip local-daemon lifecycle
// (EnsureDaemon spawns a LOCAL daemon — meaningless when the daemon is remote) and
// to suppress the local disk fallback (a remote read has no local disk to fall
// back to; surfacing the error is correct — see api/sessions.go).
func IsRemoteTarget() bool {
	url, _, _ := resolveTarget()
	return url != ""
}

// NewTargeted returns a Client for the resolved target: NewRemote against the
// remote daemon when --daemon-url/AF_DAEMON_URL is set, else New against the local
// unix socket (unchanged default). It is the single construction seam the TUI and
// CLI call instead of New(), so pointing at a remote daemon is one flag away and
// the local path is provably untouched when unset.
func NewTargeted() (*Client, error) {
	daemonURL, token, fingerprint := resolveTarget()
	if daemonURL == "" {
		return New()
	}
	return NewRemote(daemonURL, token, fingerprint)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
