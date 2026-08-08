package session

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// Composing the ssh(1) invocation for `backend = "ssh"` (#3052).
//
// This is what lets that backend run on the SAME transport as
// `backend = "sandbox"` instead of a second, in-process one. Two ssh paths is
// what generated #3044 — the same address resolving to different ports depending
// on which backend read it — and the fix there shared one helper while leaving
// the class intact. This removes the class.
//
// EVERY option below exists to reproduce what the x/crypto client did, not to
// improve on it. A convergence that quietly changed how an existing `ssh.host`
// authenticates or verifies would be the worst possible outcome: silent, and in
// the security-relevant direction.
//
// WHICH IS WHY THE COMMAND READS NO CONFIGURATION FILE — see sshNoConfigFile.

// sshNoConfigFile is `-F none`: read NEITHER ~/.ssh/config NOR
// /etc/ssh/ssh_config. It is the single most load-bearing option in this file.
//
// The first version of this convergence let ssh_config apply, on the reasoning
// that af pins every setting it owns with -o so anything else is additive. The
// pinning premise is true (`ssh -F cfg -o User=afuser -G host` does report
// `user afuser` over a host block's `User`). The conclusion is false: ssh_config
// injects BEHAVIOURS, not only values, and a pinned value cannot mask a behaviour
// that has no af-owned counterpart. Three of them, all measured against
// OpenSSH_9.6p1:
//
//   - RemoteCommand — af always supplies its own remote script, and ssh refuses
//     the combination outright: "Cannot execute command-line and remote command."
//     Every provision AND every reap fails.
//   - SendEnv — copies the DAEMON's environment VALUES to the remote, which
//     breaches af's name-only session_env_passthrough boundary. Not hypothetical:
//     a stock box already reports `sendenv LANG` / `sendenv LC_*` from
//     /etc/ssh/ssh_config, and a custom `SendEnv AWS_SECRET_ACCESS_KEY` is applied
//     verbatim.
//   - ProxyJump — spawns a CHILD ssh that inherits none of the -o host-key pins,
//     so the bastion is verified under the user's posture and its key is written
//     to ~/.ssh/known_hosts. That is exactly what #2556 forbade.
//
// And those are not the whole set: PermitLocalCommand/LocalCommand, ForwardAgent
// and ControlMaster are live too, and OpenSSH adds keywords across releases. So
// pinning the offending directives one by one is the wrong instrument — it is a
// promise to have thought of all of them, unenforced, and false on the next
// release. `-F none` is a REDUCTION, and it restores exact parity with the
// x/crypto client, which never read ssh_config either. Parity is what this
// convergence was required to preserve.
//
// An operator who genuinely needs a bastion, a ProxyCommand, or any transport af
// does not model uses `backend = "sandbox"` with a free-form sandbox_ssh command
// (#2476/#2995), which exists for precisely that. The two backends differ by WHO
// DECIDES, not by which ssh client runs.
const sshNoConfigFile = "none"

// sshCommandForConfig builds the ssh invocation for a repo's ssh.* settings and
// the operator's host-key posture.
//
// It is PURE: it composes a string and touches nothing. Callers that need the
// accept-new store to exist on disk call prepareSSHHostKeyStore separately, at
// the point they are about to run the command — restoreRuntimeCleanup composes a
// teardown during storage load and must not do I/O there (a transiently
// unwritable AF home would otherwise be captured as a permanently dead closure).
//
// WHAT IS PRESERVED, option by option:
//
//   - Configuration files: none read at all, per sshNoConfigFile above — the
//     x/crypto client read none either.
//   - User: always pinned with -o User=, to the configured ssh.user or else the
//     daemon's own account — exactly what loginUser() resolved.
//   - Port: from the shared resolver, so this backend and the sandbox/hook paths
//     cannot disagree about an address (#3044).
//   - IdentityFile: passed with -i when configured. Agent keys stay available,
//     because the old authMethods offered identity-file keys AND agent keys —
//     so IdentitiesOnly is deliberately NOT set.
//   - Host keys: the configured posture, mapped below.
//
// dialAddr is the LITERAL address every step of this session must reach (#3086).
// The caller resolves ssh.host once and passes the result here, so the pin lands
// in the command itself and no individual step can forget it. Empty means "dial
// the configured name", which is only correct where no address could be resolved
// and refusing would leak a workspace — see restoreRuntimeCleanup.
func sshCommandForConfig(cfg config.SSHConfig, posture, dialAddr string) (string, error) {
	host, port, err := resolveSSHHostPort(cfg.Host, cfg.Port)
	if err != nil {
		return "", err
	}
	if port == 0 {
		port = sshDefaultPort
	}

	knownHosts, strictOpt, err := sshHostKeyOptions(cfg, posture, host)
	if err != nil {
		return "", err
	}

	target := strings.TrimSpace(dialAddr)
	if target == "" {
		target = host
	}

	parts := []string{
		"ssh",
		// FIRST, and the reason the rest of these pins are sufficient rather than a
		// best-effort list. See sshNoConfigFile.
		"-F", sshNoConfigFile,
		"-o", "User=" + shellQuoteSandbox(sshLoginUser(cfg)),
		"-p", strconv.Itoa(port),
		"-o", "StrictHostKeyChecking=" + strictOpt,
		"-o", "UserKnownHostsFile=" + shellQuoteSandbox(knownHosts),
		// The x/crypto client consulted ONE file and never a helper program, so
		// both of these preserve today's semantics rather than tighten them.
		// Redundant under -F none, and kept deliberately: they state the invariant
		// at the only place a reader looks for it, and they survive someone later
		// deciding a config file may be read after all.
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "KnownHostsCommand=none",
		// af never had a human to ask. A prompt would hang a provision forever.
		"-o", "BatchMode=yes",
		// The command dials a literal address (#3086), so without this the host key
		// would be looked up under that ADDRESS instead of the configured name —
		// silently invalidating every existing known_hosts entry and, under
		// accept-new, writing a second one per address. HostKeyAlias restores the
		// name as the lookup key while the connection still goes to the pinned
		// address.
		//
		// The alias must be the EXACT string OpenSSH would otherwise have computed,
		// because it is used VERBATIM: measured against OpenSSH_9.6p1, a plain
		// connection on port 2201 records `[127.0.0.1]:2201`, while the same
		// connection with `HostKeyAlias=real.example` records `real.example` — the
		// port is NOT appended to an alias. So the alias is knownHostsLookupName's
		// output, which is bare on the default port and bracketed otherwise, and the
		// stored key is byte-identical to what a non-pinned connection wrote.
		"-o", "HostKeyAlias=" + shellQuoteSandbox(knownHostsLookupName(host, port)),
	}
	if identity := strings.TrimSpace(cfg.IdentityFile); identity != "" {
		// No IdentitiesOnly: authMethods offered the identity file AND agent keys,
		// and dropping the agent would break setups that rely on it.
		parts = append(parts, "-i", shellQuoteSandbox(expandUserPath(identity)))
	}
	parts = append(parts, shellQuoteSandbox(target))
	return strings.Join(parts, " "), nil
}

// sshHostKeyOptions maps ssh_host_key_verification onto the ssh binary's own
// options, preserving each posture's meaning AND its store.
//
// The store matters as much as the strictness: #2556 deliberately kept
// accept-new's learned keys OUT of the user's shared ~/.ssh/known_hosts, and
// that guarantee has to survive the transport change intact.
func sshHostKeyOptions(cfg config.SSHConfig, posture, host string) (knownHosts, strict string, err error) {
	switch posture {
	case config.SSHHostKeyInsecure:
		// Same warning the x/crypto path logged, for the same reason: this is the
		// posture under which a man-in-the-middle sees the bearer token.
		log.WarningLog.Printf("backend=ssh: host-key verification is DISABLED for %s "+
			"(ssh_host_key_verification=insecure) — a man-in-the-middle on this connection can capture "+
			"the bearer token that controls the agent session", host)
		// /dev/null is the equivalent of InsecureIgnoreHostKey: nothing is verified
		// and nothing is recorded.
		return os.DevNull, "no", nil

	case config.SSHHostKeyAcceptNew:
		// Resolve the path only. Creating the file is I/O, and this function is on
		// restoreRuntimeCleanup's composition path — see prepareSSHHostKeyStore.
		path, pathErr := acceptNewKnownHostsPathFor(cfg)
		if pathErr != nil {
			return "", "", pathErr
		}
		return path, "accept-new", nil

	default:
		// strict, and any unexpected value: fail safe to the strictest posture,
		// matching the old default branch exactly.
		path, pathErr := strictKnownHostsPathFor(cfg)
		if pathErr != nil {
			return "", "", pathErr
		}
		return path, "yes", nil
	}
}

// prepareSSHHostKeyStore creates the accept-new store if it does not exist yet,
// and does nothing for the other two postures (strict verifies against a file the
// operator owns; insecure reads /dev/null). Callers run it immediately before
// running the composed command, never while composing it.
//
// SPLIT OUT OF COMPOSITION ON PURPOSE. restoreRuntimeCleanup composes a teardown
// while persisted instances are being LOADED, under a contract that the I/O
// happens only inside the returned closure. Creating a file there means a
// transiently read-only or unavailable AF home turns one bad moment at startup
// into a permanently dead cleanup closure: the remote agent-server and workspace
// leak until the daemon restarts, and making the filesystem writable again does
// not help. Preparing per attempt costs one stat on a path that almost always
// exists, and it heals by itself.
func prepareSSHHostKeyStore(cfg config.SSHConfig, posture string) error {
	if posture != config.SSHHostKeyAcceptNew {
		return nil
	}
	path, err := acceptNewKnownHostsPathFor(cfg)
	if err != nil {
		return err
	}
	if err := ensureKnownHostsFile(path); err != nil {
		return fmt.Errorf("backend=ssh: cannot prepare host-key store %q for accept-new: %w", path, err)
	}
	return nil
}

// sshTeardownWithStore prepares the host-key store on each ATTEMPT and then
// reaps. A preparation failure is reported as unknown-state so the record is
// retained and retried — the opposite of capturing the failure once, at load.
func sshTeardownWithStore(reap func() error, cfg config.SSHConfig, posture string) func() error {
	return func() error {
		if err := prepareSSHHostKeyStore(cfg, posture); err != nil {
			return fmt.Errorf("%w: %w", ErrWorkspaceStateUnknown, err)
		}
		return reap()
	}
}

// strictKnownHostsPathFor is ssh.known_hosts if set, else the user's
// ~/.ssh/known_hosts — unchanged from before #2556.
func strictKnownHostsPathFor(cfg config.SSHConfig) (string, error) {
	if path := strings.TrimSpace(cfg.KnownHosts); path != "" {
		return expandUserPath(path), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("backend=ssh: cannot locate ~/.ssh/known_hosts (set ssh.known_hosts): %w", err)
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

// acceptNewKnownHostsPathFor is where accept-new reads and writes:
// ssh.known_hosts if the operator set it, else an af-owned file under AF_HOME.
// Never the user's ~/.ssh/known_hosts (#2556).
func acceptNewKnownHostsPathFor(cfg config.SSHConfig) (string, error) {
	if kh := strings.TrimSpace(cfg.KnownHosts); kh != "" {
		return expandUserPath(kh), nil
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("backend=ssh: cannot resolve AF home for the accept-new host-key store: %w", err)
	}
	return filepath.Join(dir, sshKnownHostsFileName), nil
}

// sshLoginUser reproduces the old loginUser(): ssh.user, else the daemon's own
// account. It is pinned into the command so ssh_config cannot change it.
func sshLoginUser(cfg config.SSHConfig) string {
	if u := strings.TrimSpace(cfg.User); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// remotePIDIdentityKillScript makes a numeric-PID reap safe: it verifies argv[0]
// is this session's unique af binary before signalling, so a recycled PID cannot
// be killed by mistake. It moved here when the x/crypto reap was deleted (#3052);
// it was always transport-agnostic, which is why one copy now serves every
// backend that reaps a remote agent-server.
func remotePIDIdentityKillScript(remotePID, afPath string) string {
	return fmt.Sprintf(
		`pid=%s; expected=%s; matches_session() { if [ -r "/proc/$pid/cmdline" ]; then actual=$(tr '\000' '\n' < "/proc/$pid/cmdline" | sed -n '1p') || return 2; [ "$actual" = "$expected" ]; return; fi; actual=$(ps -ww -p "$pid" -o command= 2>/dev/null) || return 2; actual=${actual#"${actual%%%%[![:space:]]*}"}; case "$actual" in "$expected"|"$expected "*) return 0 ;; *) return 1 ;; esac; }; if ! kill -0 "$pid" 2>/dev/null; then exit 0; fi; matches_session; matched=$?; if [ "$matched" -eq 1 ]; then exit 0; elif [ "$matched" -ne 0 ]; then exit 75; fi; kill "$pid" 2>/dev/null || exit 76; sleep 0.3; if ! kill -0 "$pid" 2>/dev/null; then exit 0; fi; matches_session; matched=$?; if [ "$matched" -eq 1 ]; then exit 0; elif [ "$matched" -ne 0 ]; then exit 75; fi; kill -9 "$pid" 2>/dev/null || exit 77`,
		shellQuote(remotePID), shellQuote(afPath))
}

// expandUserPath expands a leading ~ to the user's home dir, so ssh.identity_file
// / ssh.known_hosts accept the usual ~/.ssh/... form.
func expandUserPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
