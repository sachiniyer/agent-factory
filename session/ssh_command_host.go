package session

import (
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
