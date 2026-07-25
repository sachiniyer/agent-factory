package session

import (
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/log"
)

// agentCredentialRelPaths are the on-disk credential files each supported agent
// reads to authenticate, relative to the daemon user's home directory. Slice 2
// of #2194 bind-mounts these READ-ONLY into a docker session container (when
// docker.mount_agent_credentials is set) so a containerised agent can
// authenticate without the default-deny env boundary (#2329) granting it
// anything else.
//
// They are FILES, not directories, deliberately: (1) an agent writes its own
// state/history/token-refresh into its config dir at runtime, so a read-only
// mount of the whole dir would break it — mounting just the credential file
// leaves the surrounding dir (the container's own writable layer) intact; and
// (2) some of those dirs are enormous (~/.codex and ~/.local/share/opencode
// reach gigabytes of history/db), so a whole-dir mount would re-expose far more
// than the token. A single credential file is the minimum credential surface.
//
// The container runs with HOME=/root (dockerContainerHome), so each host file
// `~/rel` mounts to `/root/rel`, where the container's agent — a clean env, so
// default config locations — looks for it. Several candidates per agent cover
// filename differences across CLI versions (e.g. gemini's OAuth file); af
// mounts whichever exist. aider is intentionally absent: it authenticates purely
// via API-key env vars (session_env_passthrough), with no credential file to
// mount.
var agentCredentialRelPaths = []string{
	// claude — the OAuth token, plus the legacy global config that also carries
	// the logged-in account. CLAUDE_CONFIG_DIR can relocate the dir; the default
	// is covered here (relocate via run_args if you have moved it).
	".claude/.credentials.json",
	".claude.json",
	// codex — auth.json under CODEX_HOME (default ~/.codex).
	".codex/auth.json",
	// gemini — the .gemini dir under GEMINI_CLI_HOME (default ~); the OAuth
	// filename has varied across gemini-cli versions, so mount every known one.
	".gemini/oauth_creds.json",
	".gemini/gemini-credentials.json",
	".gemini/google_accounts.json",
	// amp — settings.json under ~/.config/amp (AMP_HOME) carries the token.
	".config/amp/settings.json",
	// opencode — auth.json under the XDG data dir (~/.local/share/opencode), NOT
	// the config dir (which holds no credentials).
	".local/share/opencode/auth.json",
	// devin — a cloud agent; config.json exists but the session image ships no
	// devin CLI, so this mount is a harmless no-op until an image adds devin.
	".config/devin/config.json",
}

// agentCredentialMounts returns the `-v host:container:ro` docker run arguments
// that bind-mount every credential file in agentCredentialRelPaths that exists
// under homeDir. exists is injected for testability (os.Stat in production). The
// container target is dockerContainerHome + "/" + rel — matching where the
// container's agent, running with a clean env, reads it. Host paths are absolute
// (homeDir is), so the committed config never has to carry a per-box path.
func agentCredentialMounts(homeDir string, exists func(string) bool) []string {
	if homeDir == "" {
		return nil
	}
	var args []string
	for _, rel := range agentCredentialRelPaths {
		host := filepath.Join(homeDir, rel)
		if !exists(host) {
			continue
		}
		// rel uses '/' and the container is linux, so the target is well-formed.
		target := dockerContainerHome + "/" + rel
		args = append(args, "-v", host+":"+target+":ro")
	}
	return args
}

// resolveAgentCredentialMounts resolves agentCredentialMounts against the daemon
// user's real home, stat-ing each candidate. It returns nil (mounting nothing)
// if the home cannot be resolved — an unauthenticated session is a better
// failure than aborting provisioning — and logs what it found so an operator can
// tell "mounted N creds" from "found none" when a session starts unauthenticated.
func resolveAgentCredentialMounts() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.WarningLog.Printf("backend=docker: mount_agent_credentials: cannot resolve the home dir; mounting no agent credentials: %v", err)
		return nil
	}
	mounts := agentCredentialMounts(home, func(p string) bool {
		_, statErr := os.Stat(p)
		return statErr == nil
	})
	if len(mounts) == 0 {
		log.WarningLog.Printf("backend=docker: mount_agent_credentials is set, but no known agent credential file was found under %s; sessions may start unauthenticated", home)
		return nil
	}
	// Each mount is two args ("-v", "<spec>"), so the file count is len/2.
	log.InfoLog.Printf("backend=docker: mount_agent_credentials: mounting %d agent credential file(s) read-only into the container", len(mounts)/2)
	return mounts
}
