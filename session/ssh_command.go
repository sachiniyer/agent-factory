package session

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sshrelay"
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

// sshCommandPinnedTo builds the ssh invocation for a repo's ssh.* settings and the
// operator's host-key posture, with the TCP dial pinned to one resolved address.
//
// THE ONE COMPOSER. Both the create path and the persisted-handle restore path
// build through here, because a divergence between those two is how a legacy
// handle stops being reapable (#3044) — and it is why an empty dialAddr composes
// the ordinary name-based command rather than being a separate function. Every
// caller with no address to pin wants exactly that: a cleanup handle written
// before any of this, and a create whose one-off resolution could not settle. A
// teardown that refused there would leak the workspace it exists to remove.
//
// IT TOUCHES NO FILESYSTEM STATE, which is what lets restoreRuntimeCleanup call
// it while persisted handles are loading. Callers that need the accept-new store
// to exist on disk call prepareSSHHostKeyStore separately, at the point they are
// about to run the command — a teardown composed during storage load must not do
// I/O there, or a transiently unwritable AF home is captured as a permanently
// dead closure.
//
// A PINNED command reads one thing: os.Executable, for the relay path (see
// sshPinnedProxyCommand). That is deliberately inside the boundary above rather
// than an exception to it — it names the running binary, not a path under AF home,
// so there is no transient failure for a closure to capture. Reading it fresh on
// every composition is also what keeps a teardown correct across an upgrade.
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
//     so IdentitiesOnly is deliberately NOT set. verifySSHIdentityFile is what
//     keeps that from silently authenticating as something else, and it is a
//     SEPARATE create-path step for the same reason prepareSSHHostKeyStore is.
//   - Host keys: the configured posture, mapped below.
//
// THE TARGET IS THE CONFIGURED NAME, never an address af resolved first, PINNED OR
// NOT. #3061 dialed a pinned literal address and restored the name with `-o
// HostKeyAlias` (#3086); that rejected host certificates on every non-default port
// and removed ssh's own multi-address fallback, and no alias value fixes both —
// see the block on the NO HostKeyAlias line below.
//
// So the pin goes BELOW ssh's naming layer, in `-o ProxyCommand`, which decides
// only where the socket goes. ssh computes the known_hosts key and the certificate
// principal from the name exactly as it does unpinned, and constraint 2 of #3086
// ("do not identify the host by address") is satisfied by construction rather than
// by correction. See internal/sshrelay for the measurement against a real
// certified sshd, and sshPinnedProxyCommand for what the option carries.
func sshCommandPinnedTo(cfg config.SSHConfig, posture, dialAddr string) (string, error) {
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
		// FIRST, and the reason the rest of these pins are sufficient rather than a
		// best-effort list. See sshNoConfigFile.
		"-F", sshNoConfigFile,
		"-o", "User=" + shellQuoteSandbox(sshLoginUser(cfg)),
		"-p", strconv.Itoa(port),
		"-o", "StrictHostKeyChecking=" + strictOpt,
		"-o", "UserKnownHostsFile=" + shellQuoteSandbox(knownHosts),
		// The x/crypto client consulted ONE file, so this preserves today's
		// semantics rather than tightening them.
		//
		// AND IT IS NOT REDUNDANT, which is exactly the distinction the deleted option
		// below got wrong. `-F none` stops CONFIG FILES being read; it does not touch a
		// COMPILED-IN default, and this option has one. Measured: `ssh -G -F none host`
		// still reports `globalknownhostsfile /etc/ssh/ssh_known_hosts
		// /etc/ssh/ssh_known_hosts2`, so without this a system-wide entry is consulted
		// in ADDITION to the file af pinned. It is also accepted by OpenSSH 7.4
		// (measured), so it costs no client compatibility.
		//
		// There is deliberately NO KnownHostsCommand=none beside it, and the reason
		// is a shipped outage (#3092). Its default is EMPTY — absent from
		// `ssh -G -F none` output entirely unless something sets it, and only a config
		// file could — so under `-F none` it forbade nothing that was reachable. That
		// option arrived in OpenSSH 8.5, and an unrecognised -o is not a warning — ssh
		// aborts during option parsing:
		//
		//	$ ssh -o ThisOptionDoesNotExist=none host
		//	command-line: line 0: Bad configuration option: thisoptiondoesnotexist
		//
		// So on Ubuntu 20.04 (8.2) or Debian 11 (8.4) it killed every provision,
		// tunnel and reap before a connection was attempted, whatever the posture.
		// It was pure belt-and-braces: -F none means no config file is read, so
		// nothing can install a KnownHostsCommand helper in the first place. It was
		// kept with a comment saying it "states the invariant" — which is exactly
		// how a redundant option became a hard version floor nothing documented.
		//
		// The rule this leaves behind: every option here costs a MINIMUM OpenSSH
		// VERSION, so an option that guards against nothing is not free.
		"-o", "GlobalKnownHostsFile=/dev/null",
		// af never had a human to ask. A prompt would hang a provision forever.
		"-o", "BatchMode=yes",
		// NO HostKeyAlias, and NO pinned literal address. #3090 added both to keep a
		// multi-address host from splitting a session across machines; #3092 reverted
		// them, because dialling an address forces an alias and NO ALIAS VALUE IS
		// CORRECT. Measured against a real sshd whose host key is certified for the
		// principal `real.example`:
		//
		//	HostKeyAlias=[real.example]:2202  -> cert REJECTED (alias is the principal)
		//	HostKeyAlias=real.example         -> cert accepted
		//
		// and for a PLAIN known_hosts entry on a non-default port the requirement is
		// the opposite, because OpenSSH keys those as [host]:port and uses the alias
		// verbatim. Plain entries want the bracketed form, certificates want the bare
		// name, and one string cannot be both.
		//
		// So the connection stays NAME-based: ssh resolves it, which is what keeps
		// certificates valid — and when af pins a session to one machine it does so
		// with the ProxyCommand below, which leaves this destination alone.
	}
	// The pin, when there is one. A ProxyCommand replaces ssh's TCP connect, so the
	// destination above stays the NAME and the known_hosts key and certificate
	// principal are computed from it exactly as they are unpinned.
	//
	// IT IS NOT ENTIRELY FREE, and an earlier version of this comment claimed it was
	// ("how the host is IDENTIFIED is untouched"), which was false. OpenSSH turns
	// CheckHostIP OFF whenever a ProxyCommand is set — sshconnect.c, unconditionally:
	//
	//	if (options.check_host_ip && (local ||
	//	    strcmp(hostname, ip) == 0 || options.proxy_command != NULL))
	//		options.check_host_ip = 0;
	//
	// and it DEFAULTED ON for 7.6-8.4, which is exactly the range this backend's
	// floor commits to (readconf.c sets it to 1 at V_7_6_P1/V_8_2_P1/V_8_4_P1 and to
	// 0 from V_8_5_P1, where upstream turned the default off). So on those clients a
	// pinned session loses the secondary check of the host key against the ADDRESS.
	//
	// ACCEPTED DELIBERATELY, and stated as a trade rather than a nil cost. What
	// CheckHostIP watches for is the address behind a name drifting between
	// connections — which is the very thing af now settles once and reuses for every
	// step, so the guarantee is replaced rather than dropped. Verification against
	// the NAME, which is what certificates rest on, is untouched. ProxyUseFdpass
	// does NOT recover it and was checked rather than assumed: it clears the version
	// bar (present at V_7_6_P1) but is reached only INSIDE the branch where
	// proxy_command is set, and the disable above keys on precisely that.
	if pinned := strings.TrimSpace(dialAddr); pinned != "" {
		proxy, proxyErr := sshPinnedProxyCommand(pinned, port)
		if proxyErr != nil {
			return "", proxyErr
		}
		parts = append(parts, "-o", proxy)
	}
	if identity := strings.TrimSpace(cfg.IdentityFile); identity != "" {
		// Composition stays FILESYSTEM-PURE — the readability check lives in
		// verifySSHIdentityFile, called from the provision path. See there.
		parts = append(parts, "-i", shellQuoteSandbox(expandUserPath(identity)))
	}
	parts = append(parts, shellQuoteSandbox(host))
	return strings.Join(parts, " "), nil
}

// verifySSHIdentityFile refuses an ssh.identity_file af cannot actually hand to
// ssh, rather than letting ssh warn and carry on (#3092).
//
// ssh treats an unusable -i as advisory:
//
//	Warning: Identity file /missing not accessible: No such file or directory.
//	…and it proceeds, authenticating with agent/default keys instead.
//
// IdentitiesOnly is deliberately OFF (the old authMethods offered the identity
// file AND agent keys), so that fallback is silent: a typo in ssh.identity_file
// authenticates as somebody else. The old in-process client errored on an
// explicit file it could not read.
//
// It OPENS the file rather than stat-ing it, and requires a regular file. A stat
// succeeds for a directory, for a mode-000 file, and for a socket — none of which
// ssh can load as a key, so each would fall straight back to the agent, which is
// the failure this exists to prevent.
//
// IT DOES NOT VALIDATE THE KEY'S CONTENT, and that is a decision rather than an
// omission. A malformed file does reach the same silent fallback — measured,
// `Load key "x": error in libcrypto` and ssh carries on with other identities —
// but every way of checking content is worse than the gap it closes: parsing key
// formats here would have to accept unencrypted and encrypted PEM, the OpenSSH
// format, certificates, PKCS#11 and FIDO tokens, and whatever OpenSSH adds next,
// and `ssh-keygen -y` cannot read an encrypted key without its passphrase. Any of
// those would REJECT working configurations, which is a worse failure than the
// one being prevented. Note also that ssh does print a diagnostic for a malformed
// key, so that case is not silent in the way a missing path was.
//
// So the guard's scope is exactly what it can decide safely: this path names a
// real, readable, regular file that af can hand to ssh. Catching a typo is the
// job; adjudicating cryptography is not.
//
// CALLED FROM PROVISION ONLY, and deliberately not from the teardown path.
// restoreRuntimeCleanup composes a closure while persisted handles are loading,
// and a check there would capture a permanently dead cleanup the moment a key is
// briefly unavailable — the #3061 defect this file already fixed once for the
// accept-new store. Nor does the teardown gain from it: a reap authenticating
// with the wrong identity fails and RETAINS, while refusing to compose a teardown
// leaks the workspace it exists to remove (the #3044 lesson). The wrong-identity
// risk is a CREATE-time risk, so the check lives at create time.
func verifySSHIdentityFile(cfg config.SSHConfig) error {
	identity := strings.TrimSpace(cfg.IdentityFile)
	if identity == "" {
		return nil
	}
	path := expandUserPath(identity)
	refuse := func(why error) error {
		return fmt.Errorf("backend=ssh: ssh.identity_file %q cannot be used as an ssh key: %w "+
			"(af refuses rather than silently falling back to your agent or default keys, "+
			"which would authenticate as a different identity than the one configured)", path, why)
	}
	// KIND FIRST, THEN READABILITY — this order is load-bearing, not tidiness.
	// Opening a FIFO for reading BLOCKS until another process opens the write end,
	// and there is no writer for a stray pipe in ~/.ssh. Checking the kind by
	// OPENING first therefore hangs the provision forever instead of refusing it,
	// which is strictly worse than the silent fallback this function exists to stop.
	// Measured: the open-first form hung this package's own test on a named pipe
	// until the suite was killed. A stat cannot block, so the kind is settled with
	// one.
	info, err := os.Stat(path)
	if err != nil {
		return refuse(err)
	}
	if !info.Mode().IsRegular() {
		return refuse(fmt.Errorf("it is not a regular file (mode %s)", info.Mode()))
	}

	// Now prove the daemon can actually READ it — a mode-000 file stats perfectly
	// well, and only an open distinguishes it. O_NONBLOCK because the stat above
	// cannot rule out the path becoming a pipe in between: a daemon that refuses is
	// recoverable, one that hangs is not.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return refuse(err)
	}
	return f.Close()
}

// sshPinnedProxyCommand builds the `-o ProxyCommand=…` that makes ssh's TCP
// connect land on ONE address while ssh's destination stays the configured name.
//
// The relay is af's OWN binary. `nc`/`socat` would be the same failure class as an
// ssh option that is too new — a dependency af cannot guarantee, whose absence
// breaks the whole backend rather than degrading — while af is present by
// definition, since af is the process composing this command. It is resolved
// fresh rather than persisted, so a daemon that restarted into an upgraded binary
// names the one it is actually running.
//
// A FAILURE HERE IS RETURNED, not swallowed, and that is the opposite of the
// empty-dialAddr case above. Reaching this function means a session IS pinned to a
// machine — on the teardown path, a record that knows the exact host its workspace
// is on. Quietly composing a name-based command there could reap a DIFFERENT
// machine, find nothing, report success and retire the only tombstone: silent and
// permanent. The error instead reaches restoreRuntimeCleanup, which classifies the
// handle as unavailable, so the record is RETAINED and retried. Retained-and-
// retried beats silently-wrong-and-retired.
//
// TWO LAYERS OF QUOTING, because the string passes through two shells, and NEITHER
// is optional:
//
//   - ssh runs a ProxyCommand as `/bin/sh -c <value>`, so the relay path is
//     shell-quoted INSIDE the value — an AF home with a space would otherwise
//     split into a command and a stray argument.
//   - af runs the whole ssh invocation as `sh -c '<sshCmd> "$@"'`, so the value is
//     shell-quoted AGAIN for that outer shell, which is what keeps it one argv
//     element on the way to ssh.
//
// AND A THIRD ESCAPE THAT IS NOT QUOTING AT ALL. ssh percent-expands a
// ProxyCommand before running it, so a literal `%` in the relay path or in a
// zoned IPv6 address (`fe80::1%eth0`) is read as a token. Measured on
// OpenSSH_9.6p1: an unescaped `%d` aborts with `vdollar_percent_expand: unknown
// key %d` and the connection never starts, while `%%` yields a literal `%`. The
// `%%` case is in percent_expand at the 7.6 floor, so the escape costs no version.
//
// NO `%h`/`%p` TOKENS, deliberately: they expand to the values ssh is dialling,
// which is the name — and the entire point is that the socket goes somewhere the
// name does not necessarily lead.
func sshPinnedProxyCommand(dialAddr string, port int) (string, error) {
	relay, err := sshRelayBinary()
	if err != nil {
		return "", fmt.Errorf("backend=ssh: cannot locate the af binary to relay this session's pinned "+
			"connection to %s (af runs itself as ssh's ProxyCommand so every step reaches the one machine "+
			"the session was provisioned on): %w", dialAddr, err)
	}
	inner := shellQuoteSandbox(relay) +
		" " + sshrelay.Subcommand +
		" " + shellQuoteSandbox(dialAddr) +
		" " + strconv.Itoa(port)
	return "ProxyCommand=" + shellQuoteSandbox(escapeSSHPercent(inner)), nil
}

// escapeSSHPercent doubles every `%` so ssh's percent expansion yields the literal
// string back. See sshPinnedProxyCommand for the measurement.
func escapeSSHPercent(s string) string {
	return strings.ReplaceAll(s, "%", "%%")
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
