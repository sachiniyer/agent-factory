package session

import (
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// agentCredentialFiles maps a supported agent to the on-disk credential file(s)
// it reads to authenticate, relative to the daemon user's home directory. When
// the operator grant docker_mount_agent_credentials is set, #2194 bind-mounts
// ONLY the file(s) for the session's OWN resolved agent (agentCredentialMounts
// is called with p.agentName()) read-only into the container — never another
// agent's credential. A codex session must not receive the live Claude token.
//
// Files, not directories, deliberately: (1) an agent writes its own
// state/history/token-refresh into its config dir at runtime, so a read-only
// mount of the whole dir would break it — mounting just the credential file
// leaves the surrounding dir (the container's own writable layer) intact; and
// (2) some of those dirs reach gigabytes of history/db (~/.codex,
// ~/.local/share/opencode), which a whole-dir mount would re-expose. A single
// credential file is the minimum credential surface.
//
// The container runs with HOME=/root (dockerContainerHome), so each host file
// `~/rel` mounts to `/root/rel`, where the container's agent — a clean env, so
// default config locations — looks for it. Several candidates per agent cover
// filename differences across CLI versions (e.g. gemini's OAuth file); af mounts
// whichever exist.
//
// Deliberate exclusions:
//   - aider: authenticates purely via API-key env vars (name them in
//     session_env_passthrough); no credential file to mount.
//   - claude's ~/.claude.json is NOT here: it is not a credential (that is
//     ~/.claude/.credentials.json) — it is the config/privacy blob (mcpServers
//     with third-party tokens, project paths + prompt history, account/machine
//     ids), and it is rewritten on every claude start, so a :ro single-file bind
//     is both useless for auth and breakage-prone.
var agentCredentialFiles = map[string][]string{
	tmux.ProgramClaude: {".claude/.credentials.json"},
	tmux.ProgramCodex:  {".codex/auth.json"},
	tmux.ProgramGemini: {
		".gemini/oauth_creds.json",
		".gemini/gemini-credentials.json",
		".gemini/google_accounts.json",
	},
	tmux.ProgramAmp:      {".config/amp/settings.json"},
	tmux.ProgramOpencode: {".local/share/opencode/auth.json"},
	// devin is a cloud agent; config.json holds its settings but the session
	// image ships no devin CLI, so this is a no-op until an image adds devin.
	tmux.ProgramDevin: {".config/devin/config.json"},
}

// agentCredentialMounts returns the `-v host:container:ro` docker run arguments
// that bind-mount the credential file(s) for the single agent `agent` that exist
// under homeDir. exists is injected for testability (os.Stat in production). The
// container target is dockerContainerHome + "/" + rel — matching where the
// container's agent, running with a clean env, reads it. Host paths are absolute
// (homeDir is), so the config never has to carry a per-box path.
func agentCredentialMounts(agent, homeDir string, exists func(string) bool) []string {
	if homeDir == "" {
		return nil
	}
	var args []string
	for _, rel := range agentCredentialFiles[agent] {
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

// resolveAgentCredentialMounts resolves agentCredentialMounts for the session's
// resolved agent against the daemon user's real home, stat-ing each candidate.
// It returns nil (mounting nothing) if the home cannot be resolved — an
// unauthenticated session is a better failure than aborting provisioning — and
// logs what it found so an operator can tell "mounted the codex credential" from
// "found none" when a session starts unauthenticated.
func resolveAgentCredentialMounts(agent string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.WarningLog.Printf("backend=docker: docker_mount_agent_credentials: cannot resolve the home dir; mounting no credential for %q: %v", agent, err)
		return nil
	}
	mounts := agentCredentialMounts(agent, home, func(p string) bool {
		_, statErr := os.Stat(p)
		return statErr == nil
	})
	if len(mounts) == 0 {
		log.WarningLog.Printf("backend=docker: docker_mount_agent_credentials is set, but no credential file for agent %q was found under %s; the session may start unauthenticated", agent, home)
		return nil
	}
	// Each mount is two args ("-v", "<spec>"), so the file count is len/2.
	log.InfoLog.Printf("backend=docker: docker_mount_agent_credentials: mounting %d credential file(s) for agent %q read-only into the container", len(mounts)/2, agent)
	return mounts
}
