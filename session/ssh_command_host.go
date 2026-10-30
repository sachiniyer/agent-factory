package session

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// Composing ssh for a NAMED host (#5571 W6.2).
//
// A host-list entry reaches ssh the way `backend = "ssh"` does: af writes the
// whole command, reads no ssh_config (-F none), enforces a host-key posture, and
// can pin every step to one resolved machine through the same relay
// ProxyCommand, so a named host is always one machine. It goes through the ONE
// composer, sshCommandPinnedTo, rather than a second one that could disagree.
//
// The posture is strict unless the operator says otherwise: the host's own
// host_key_verification, else the global ssh.host_key_verification (whose
// default is strict).
//
// A host with use_ssh_config = true is the explicit opt-in to the operator's
// ssh_config — the sandbox.ssh trust model. af then composes no -F none and no
// host-key options (the operator's file is the authority), but still passes the
// pin and the fields the entry sets. This composer touches no filesystem state,
// for the reason sshCommandPinnedTo gives.

// sshConfigForHost maps a host-list entry onto the ssh settings the shared
// composer understands. KnownHosts stays empty: the operator's known_hosts (or
// the af-owned accept-new store) applies, as for ssh.host.
func sshConfigForHost(h config.HostConfig) config.SSHConfig {
	return config.SSHConfig{
		Host:         h.Host,
		User:         h.User,
		Port:         h.Port,
		IdentityFile: h.IdentityFile,
	}
}

// hostPosture is the host-key posture af enforces for h.
func hostPosture(h config.HostConfig, globalPosture string) string {
	if h.HostKeyVerification != "" {
		return h.HostKeyVerification
	}
	if globalPosture == "" {
		return config.SSHHostKeyStrict
	}
	return globalPosture
}

// sshCommandForHost builds the ssh invocation (without a remote command) for a
// listed host. dialAddr/dialPort pin the TCP dial to one machine, as in
// sshCommandPinnedTo; empty/zero composes the ordinary name-based command.
func sshCommandForHost(h config.HostConfig, globalPosture, dialAddr string, dialPort int) (string, error) {
	cfg := sshConfigForHost(h)
	if !h.UseSSHConfig {
		return sshCommandPinnedTo(cfg, hostPosture(h, globalPosture), dialAddr, dialPort)
	}

	host, port, err := resolveSSHHostPort(cfg.Host, cfg.Port)
	if err != nil {
		return "", err
	}
	// No -F: the operator's ssh_config applies, which is what the opt-in means.
	parts := []string{"ssh", "-o", "BatchMode=yes"}
	if u := strings.TrimSpace(cfg.User); u != "" {
		parts = append(parts, "-o", shellQuote("User="+sshConfigQuote(u)))
	}
	if port != 0 {
		parts = append(parts, "-p", strconv.Itoa(port))
	}
	if pinned := strings.TrimSpace(dialAddr); pinned != "" {
		pinnedPort := port
		if pinnedPort == 0 {
			pinnedPort = sshDefaultPort
		}
		if dialPort != 0 {
			pinnedPort = dialPort
		}
		proxy, err := sshPinnedProxyCommand(pinned, pinnedPort)
		if err != nil {
			return "", err
		}
		parts = append(parts, "-o", proxy)
	}
	if identity := strings.TrimSpace(cfg.IdentityFile); identity != "" {
		parts = append(parts, "-i", shellQuote(expandUserPath(identity)))
	}
	parts = append(parts, shellQuote(host))
	return strings.Join(parts, " "), nil
}

// OperatorSSHCommand is the ssh invocation af itself uses for the listed host h,
// without a remote command: what an operator runs (append a command such as
// `true`) to test the very connection af will make. Printed remedies use this
// rather than formatting `ssh user@host:port` by hand, which is not ssh syntax —
// OpenSSH reads `box:2222` as a hostname on port 22 (#5852). The composition is
// pure: it touches no filesystem state and dials nothing.
func OperatorSSHCommand(h config.HostConfig, globalPosture string) (string, error) {
	return sshCommandForHost(h, globalPosture, "", 0)
}

// chooseListedHost resolves which listed host a create uses: the named pick, or
// else — only when the repo supplies no [ssh] host of its own — the global
// default_host. It returns a nil host for "use the repo's [ssh] settings".
//
// An unknown name is refused with the configured names, before any remote state
// exists. A host that opts into ssh_config is refused here too: this path
// composes with -F none, and the sandbox transport that honors the opt-in is a
// later slice, so accepting it would silently ignore the operator's choice.
func chooseListedHost(pick string, cfg *config.ResolvedConfig) (string, *config.HostConfig, error) {
	name := strings.TrimSpace(pick)
	if name == "" {
		if cfg.SSH != nil && strings.TrimSpace(cfg.SSH.Host) != "" {
			return "", nil, nil
		}
		name = cfg.DefaultHost
	}
	if name == "" {
		return "", nil, nil
	}
	if err := config.ValidateHostPick(cfg.Hosts, name); err != nil {
		return "", nil, fmt.Errorf("backend=ssh: %w", err)
	}
	entry := cfg.Hosts[name]
	// The same validation `af config set` and the loader run, so an entry that
	// reached cfg some other way (a test, a stale in-memory copy) cannot be
	// created against, and the catalog cannot call it usable.
	if err := config.ValidateHostEntry(name, entry); err != nil {
		return "", nil, fmt.Errorf("backend=ssh: %w", err)
	}
	if _, _, err := resolveSSHHostPort(entry.Host, entry.Port); err != nil {
		return "", nil, fmt.Errorf("backend=ssh: host %q: %w", name, err)
	}
	if entry.UseSSHConfig {
		return "", nil, fmt.Errorf("backend=ssh: host %q sets use_ssh_config, which sessions cannot use yet (this path composes ssh itself); remove use_ssh_config for this host", name)
	}
	return name, &entry, nil
}

// HostUnusableReason reports why a create picking the listed host name would
// fail right now, or nil when every locally checkable precondition passes. It is
// the choose-time twin of chooseListedHost + sshRuntime.Provision, so a picker
// never offers a host a create will refuse. It dials nothing.
func HostUnusableReason(name string, cfg *config.ResolvedConfig, repoRoot string) error {
	if cfg == nil {
		return fmt.Errorf("the config could not be read")
	}
	if _, listed, err := chooseListedHost(name, cfg); err != nil {
		return err
	} else if listed == nil {
		return fmt.Errorf("host %q is not in the host list", name)
	} else if _, err := lookPath("ssh"); err != nil {
		return sshCLIMissingError(err)
	} else if err := verifySSHIdentityFile(sshConfigForHost(*listed)); err != nil {
		return err
	}
	if originRemoteURL(repoRoot) == "" {
		return missingOriginError(BackendSSH, repoRoot)
	}
	return nil
}
