package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// Backend config preconditions (#1933). docker, ssh, and hook each need a key in
// the repo's own .agent-factory config before a session can run on them:
// docker.image, ssh.host, and remote_hooks respectively. Those requirements were
// previously stated only inside each runtime's Provision, which meant they could
// only be discovered by trying to create a session and reading the failure.
//
// BackendConfigError states the same requirement from the repo config ALONE, with
// no provisioning and no side effects, so a client can ask "could this repo use
// docker?" before offering the choice. The runtimes call it as their first
// config-dependent check, so the message a user reads at choose time in the web is
// the identical message the CLI prints at create time — one string, one place,
// no drift.
//
// Scope is deliberately repo CONFIG only. Preconditions that depend on the
// environment or the session (the `docker` CLI being on PATH, the repo having an
// `origin` remote to clone from) stay in Provision: they are not answerable from
// config, and a client that pre-checked them would be asserting things it cannot
// know. So a backend reported available here can still fail to provision — this
// narrows the failure surface to the environment, it does not eliminate it.

// BackendConfigError reports why kind cannot be used in the repo whose resolved
// config is cfg, or nil when the repo config satisfies the backend's
// requirements. A nil cfg is treated as an empty config (every optional section
// absent), which is the correct reading for a repo with no in-repo config file.
//
// The returned error is user-facing and actionable: it names the missing key and
// the file it belongs in.
func BackendConfigError(kind BackendKind, cfg *config.ResolvedConfig) error {
	switch kind {
	case BackendDocker:
		if cfg == nil || cfg.Docker == nil || strings.TrimSpace(cfg.Docker.Image) == "" {
			return fmt.Errorf("backend=docker requires docker.image to be set in this repo's .agent-factory/config.json (the container image that carries git + tmux + the agent CLIs; the `af` binary is copied in automatically)")
		}
	case BackendSSH:
		if cfg == nil || cfg.SSH == nil || strings.TrimSpace(cfg.SSH.Host) == "" {
			return fmt.Errorf("backend=ssh requires ssh.host to be set in this repo's .agent-factory/config.json (the remote host the session's workspace + agent run on)")
		}
	case BackendHook:
		if cfg == nil || cfg.RemoteHooks == nil {
			return fmt.Errorf("backend=hook requires remote_hooks to be configured in this repo's .agent-factory/config.json (the launch/delete commands that provision the session on your own infrastructure)")
		}
	}
	// BackendLocal needs nothing from the repo config, and an unregistered kind is
	// not this function's error to raise — ParseBackendKind rejects those. A new
	// backend with no config requirement correctly reports available by default.
	return nil
}
