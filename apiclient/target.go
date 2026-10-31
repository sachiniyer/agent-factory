package apiclient

import "os"

// Remote-target resolution (#1592 Phase 3 PR4, §1.6). A TUI/CLI points at a
// remote daemon with flags, falling back to env, then the saved target; unset ⇒ the local unix socket
// (today's behavior, unchanged). The precedence is flag > env, resolved lazily at
// each client-construction site so a flag parsed by the root command and an env
// var in a bare `af sessions ...` invocation both take effect through one path.
//
// Since W9.6 there IS a client-side saved target (savedtarget.go, `af target`),
// as the lowest-precedence source: flag, then env, then the saved file. It holds
// at most one target; a per-repo/global config layer would only add ambiguity.

// Env-var names for the remote target, the fallback when the matching flag is
// unset. Named on the AF_DAEMON_* namespace so they read as "which daemon".
const (
	envDaemonURL   = "AF_DAEMON_URL"
	envDaemonToken = "AF_DAEMON_TOKEN"
)

// Flag-backed remote-target values, bound by the root command's persistent flags
// (commands/root.go). Empty ⇒ fall back to the matching env var. These are the
// ONLY package-level mutable state the target resolver reads; a test sets them
// directly (and restores them) to exercise the remote path without cobra.
var (
	FlagDaemonURL   string
	FlagDaemonToken string
)

// resolveTargetFull merges flag > env > saved into the effective target and says
// which source won. An empty URL with a nil error means "no remote target" — the
// caller uses the local unix socket. A non-nil error means the SAVED target
// failed closed (see savedtarget.go); URL is then empty but the target must be
// treated as remote so nothing quietly acts on the local daemon.
//
// The token follows the URL's source: a flag/env URL takes only the flag/env
// token, never the saved one.
func resolveTargetFull() (daemonURL, token string, src TargetSource, err error) {
	flagOrEnvURL := firstNonEmpty(FlagDaemonURL, os.Getenv(envDaemonURL))
	flagOrEnvToken := firstNonEmpty(FlagDaemonToken, os.Getenv(envDaemonToken))
	if flagOrEnvURL != "" {
		src = TargetEnv
		if FlagDaemonURL != "" {
			src = TargetFlag
		}
		return flagOrEnvURL, flagOrEnvToken, src, nil
	}
	saved, err := LoadSavedTarget()
	if err != nil {
		return "", "", TargetSaved, err
	}
	if saved != nil {
		// An explicit token (flag/env) still overrides the saved one for the
		// saved URL, e.g. a rotated token supplied for one invocation.
		return saved.URL, firstNonEmpty(flagOrEnvToken, saved.Token), TargetSaved, nil
	}
	return "", flagOrEnvToken, TargetLocal, nil
}

// resolveTarget is resolveTargetFull without the diagnostics, for callers that
// only branch on remote-or-not.
func resolveTarget() (daemonURL, token string) {
	daemonURL, token, _, _ = resolveTargetFull()
	return
}

// HasDaemonToken reports whether the client will present a daemon credential:
// the --token flag, falling back to AF_DAEMON_TOKEN, exactly as resolveTarget
// merges them. Callers that branch on "am I authenticated as a sandbox" must ask
// this rather than read the environment, or a flag-supplied credential takes the
// unauthenticated path (#5747).
func HasDaemonToken() bool {
	// Flag/env only: a SAVED operator token is not a sandbox identity.
	return firstNonEmpty(FlagDaemonToken, os.Getenv(envDaemonToken)) != ""
}

// IsRemoteTarget reports whether a remote daemon target is configured (a non-empty
// --daemon-url / AF_DAEMON_URL). Callers use it to skip local-daemon lifecycle
// (EnsureDaemon spawns a LOCAL daemon — meaningless when the daemon is remote) and
// to suppress the local disk fallback (a remote read has no local disk to fall
// back to; surfacing the error is correct — see api/sessions.go).
func IsRemoteTarget() bool {
	url, _, _, err := resolveTargetFull()
	return url != "" || err != nil
}

// RemoteTargetURL returns the resolved remote daemon URL, or "" when none is
// configured (the local unix socket). It is IsRemoteTarget's companion for the
// callers that must NAME the target rather than merely branch on it: a message
// about a remote write says which daemon it reached, so an operator reading it
// on the wrong machine can tell at a glance (#3679).
func RemoteTargetURL() string {
	url, _ := resolveTarget()
	return url
}

// NewTargeted returns a Client for the resolved target: NewRemote against the
// remote daemon when --daemon-url/AF_DAEMON_URL is set, else New against the local
// unix socket (unchanged default). It is the single construction seam the TUI and
// CLI call instead of New(), so pointing at a remote daemon is one flag away and
// the local path is provably untouched when unset.
func NewTargeted() (*Client, error) {
	daemonURL, token, _, err := resolveTargetFull()
	if err != nil {
		return nil, err
	}
	if daemonURL == "" {
		return New()
	}
	return NewRemote(daemonURL, token)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
