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
// The one DELIBERATE difference is ssh_config, and it is documented rather than
// hidden — see sshCommandForConfig.

// sshCommandForConfig builds the ssh invocation for a repo's ssh.* settings and
// the operator's host-key posture.
//
// WHAT IS PRESERVED, option by option:
//
//   - User: always pinned with -o User=, to the configured ssh.user or else the
//     daemon's own account — exactly what loginUser() resolved. Pinning it means
//     an ssh_config `User` directive cannot silently change who af logs in as.
//   - Port: from the shared resolver, so this backend and the sandbox/hook paths
//     cannot disagree about an address (#3044).
//   - IdentityFile: passed with -i when configured. Agent keys stay available,
//     because the old authMethods offered identity-file keys AND agent keys —
//     so IdentitiesOnly is deliberately NOT set.
//   - Host keys: the configured posture, mapped below.
//
// WHAT CHANGES, deliberately: ~/.ssh/config now applies. The x/crypto client
// never read it, so a `Host` block matching ssh.host had no effect; now its
// HostName, ProxyJump, ProxyCommand and friends do. That is the capability this
// convergence exists to deliver — a bastion is reachable without switching
// backends — and it is the one behavioural change an existing user can notice.
// af's own settings still win, because every one of them is pinned with -o.
func sshCommandForConfig(cfg config.SSHConfig, posture string) (string, error) {
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

	parts := []string{
		"ssh",
		"-o", "User=" + shellQuoteSandbox(sshLoginUser(cfg)),
		"-p", strconv.Itoa(port),
		"-o", "StrictHostKeyChecking=" + strictOpt,
		"-o", "UserKnownHostsFile=" + shellQuoteSandbox(knownHosts),
		// The x/crypto client consulted ONE file and never a helper program, so
		// both of these preserve today's semantics rather than tighten them: without
		// them /etc/ssh/ssh_known_hosts or an ssh_config KnownHostsCommand could
		// satisfy verification that af's own store was supposed to decide.
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "KnownHostsCommand=none",
		// af never had a human to ask. A prompt would hang a provision forever.
		"-o", "BatchMode=yes",
	}
	if identity := strings.TrimSpace(cfg.IdentityFile); identity != "" {
		// No IdentitiesOnly: authMethods offered the identity file AND agent keys,
		// and dropping the agent would break setups that rely on it.
		parts = append(parts, "-i", shellQuoteSandbox(expandUserPath(identity)))
	}
	parts = append(parts, shellQuoteSandbox(host))
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
		path, pathErr := acceptNewKnownHostsPathFor(cfg)
		if pathErr != nil {
			return "", "", pathErr
		}
		if ensureErr := ensureKnownHostsFile(path); ensureErr != nil {
			return "", "", fmt.Errorf("backend=ssh: cannot prepare host-key store %q for accept-new: %w", path, ensureErr)
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
