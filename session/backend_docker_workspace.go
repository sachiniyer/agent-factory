package session

import (
	"fmt"
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// prepareAfGitConfig creates the fresh global gitconfig af's own
// credentialed git reads inside the container (#7891 R1): the provisioning
// preamble exports GIT_CONFIG_GLOBAL at it and the agent-server's own git
// is pointed at it through AF_GIT_CONFIG. An af-created file is the only
// way to guarantee no image- or repo-authored config entry — http.sslVerify,
// an include.path, a credential helper — reaches a git run carrying af's
// credential. Skipped when the session carries no credential and when the
// operator claimed the git-config route (no lockdown, no file).
func (p *dockerProvisioner) prepareAfGitConfig() error {
	if len(p.credentialMounts) == 0 && p.spec.Account.Dir == "" {
		return nil
	}
	if !afGitConfigActive(p.afGitConfigClaims(p.forwardedEnvNames())) {
		return nil
	}
	file := p.afGitConfigFile()
	out, err := p.execSessionSh(dockerShortStepTimeout, ": > "+shellQuote(file))
	if err != nil {
		return fmt.Errorf("backend=docker: creating af's gitconfig in the container failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// trustWorkspaceDirectories marks every directory safe in the CONTAINER's own
// global git config, so the agent-server's worktree work over a possibly
// foreign-owned bind mount does not trip on "dubious ownership". The config
// lives in the container's home, not the operator's, so W2.7's rule about the
// host's gitconfig does not apply; it runs before the clone, as it always did.
//
// Under the git-config lockdown the write splits: `--global` would resolve
// the preamble's GIT_CONFIG_GLOBAL — which the preamble points at af's own
// file, the claimed XDG file, or leaves as the operator's forwarded global,
// exactly the file af's credentialed git will read — while the agent's own
// git keeps reading the file master's write hit (agentGitConfigFile), which
// gets the same mark through an explicit --file (#7891 R4). A claimed path
// may be absent or read-only inside the container: the directories are
// created when missing, and the writes are best-effort because a claimed
// file may already carry the mark or be unwritable on a mount the
// operator owns — a failed write must not abort a session their file
// already serves, and without the mark the clone's own failure message
// names the remedy (Codex on #7891 N1). When the container's
// GIT_CONFIG_GLOBAL is defined-empty the agent's git reads no global
// file at all — there is no target to mark, so the step writes only
// af's effective global and warns rather than naming a file the agent
// ignores (#7949 review R2).
func (p *dockerProvisioner) trustWorkspaceDirectories() error {
	script := `git config --global --add safe.directory "*"`
	if len(p.credentialMounts) > 0 || p.spec.Account.Dir != "" {
		if claimed := p.afGitConfigClaims(p.forwardedEnvNames()); afGitConfigActive(claimed) {
			if file, ok := p.agentGitConfigFile(); ok {
				agentFile := shellQuote(file)
				agentMkdir, agentSuffix := "", ""
				if claimed["GIT_CONFIG_GLOBAL"] {
					// The agent's global is the operator's file under this
					// claim — absent- or read-only-mounted like af's side.
					agentMkdir = fmt.Sprintf(`mkdir -p "$(dirname %s)" 2>/dev/null || true; `, agentFile)
					agentSuffix = " 2>/dev/null || true"
				}
				script = fmt.Sprintf(
					`mkdir -p "$(dirname "$GIT_CONFIG_GLOBAL")" 2>/dev/null || true; git config --file "$GIT_CONFIG_GLOBAL" --add safe.directory "*" 2>/dev/null || true; %sgit config --file %s --add safe.directory "*"`,
					agentMkdir, agentFile) + agentSuffix
			} else {
				script = `mkdir -p "$(dirname "$GIT_CONFIG_GLOBAL")" 2>/dev/null || true; git config --file "$GIT_CONFIG_GLOBAL" --add safe.directory "*" 2>/dev/null || true`
				log.WarningLog.Printf(
					"backend=docker: the container's GIT_CONFIG_GLOBAL is defined-empty, so the agent's git reads no global file and safe.directory could not be written to a scope it reads — the agent's git may still report 'dubious ownership'; unset the empty GIT_CONFIG_GLOBAL or point it at a writable file")
			}
		}
	}
	out, err := p.execSessionSh(dockerShortStepTimeout, script)
	if err != nil {
		return fmt.Errorf("backend=docker: git config in container failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// configureGit writes the committer identity into the clone's repo-local git
// config inside the container. It runs after the clone and writes no global
// identity (#5571 W2.7).
func (p *dockerProvisioner) configureGit() error {
	script := gitIdentityCommand(dockerWorkspaceDir, p.spec.GitUserName, p.spec.GitUserEmail)
	out, err := p.execSessionSh(dockerShortStepTimeout, script)
	if err != nil {
		return fmt.Errorf("backend=docker: git config in container failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// cloneWorkspace clones the repo's origin into /workspace inside the container.
// A fresh create clones the default branch; the in-container agent-server's LOCAL
// backend then creates the session's git worktree + branch off it. On a RESTORE
// (spec.RestoreBranch set, #1592 Phase 4 PR6) it additionally materializes the
// pushed session branch as a local ref so the in-container Setup checks it out.
func (p *dockerProvisioner) cloneWorkspace() error {
	script := gitCloneCommand(p.spec.CloneURL, dockerWorkspaceDir, p.cloneTemplateSuppression())
	out, err := p.execSessionSh(dockerProvisionStepTimeout, script)
	if err != nil {
		hint := ""
		if len(p.credentialMounts) > 0 || p.spec.Account.Dir != "" {
			// A TLS failure here most often means the clone needed a CA or
			// git config the lockdown withheld — name the documented
			// operator channel rather than leaving it to guess (#7891 R5).
			hint = "; af locks down the clone's TLS trust and git config to af-created files on a credential-carrying session — if it needs a custom CA, proxy, or git config, mount the file with docker.run_args (-v) and forward the name through session_env_passthrough"
		}
		return fmt.Errorf("backend=docker: cloning %q into the container failed (is git in the image, and the URL reachable from inside the container?%s): %s: %w",
			p.spec.CloneURL, hint, strings.TrimSpace(string(out)), err)
	}
	if credentialCommand := gitPersistCredentialCommand(p.spec.CloneURL, dockerWorkspaceDir); credentialCommand != "" {
		out, err = p.execSessionSh(dockerShortStepTimeout, credentialCommand)
		if err != nil {
			return fmt.Errorf("backend=docker: configuring value-free GitHub credentials in the clone failed: %s: %w",
				strings.TrimSpace(string(out)), err)
		}
	}
	if branch := strings.TrimSpace(p.spec.RestoreBranch); branch != "" {
		return p.fetchRestoreBranch(branch)
	}
	return nil
}

// fetchRestoreBranch materializes the archived session branch (pushed to origin
// at archive time) as a LOCAL ref in the fresh clone, WITHOUT checking it out in
// /workspace's main tree (#1592 Phase 4 PR6). The `<branch>:<branch>` refspec
// creates refs/heads/<branch> from origin/<branch>; the in-container local
// backend's Setup then finds that local ref and adds the session worktree from
// it (setupFromExistingBranch), bringing the pushed commits back. It stays on the
// main tree's default branch so the later `git worktree add <path> <branch>` does
// not collide with a checked-out branch.
func (p *dockerProvisioner) fetchRestoreBranch(branch string) error {
	script := fmt.Sprintf("git -C %s fetch origin %s:%s",
		shellQuote(dockerWorkspaceDir), shellQuote(branch), shellQuote(branch))
	out, err := p.execSessionSh(dockerProvisionStepTimeout, script)
	if err != nil {
		return fmt.Errorf("backend=docker: restoring archived branch %q into the container failed (was it pushed to origin?): %s: %w",
			branch, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// copyAfBinary delivers the `af` binary into the container on the cp fallback
// path — the sessions whose staged binds could not mount (Docker Desktop file
// sharing) or whose payload could not be staged. When the staged copy exists
// `docker cp` preserves its 0755 mode and the root chmod is unnecessary; when
// staging itself failed the pre-#7577 path runs whole — cp of the daemon's own
// binary plus the chmod that makes it exec'able by the session user. The
// binary must be compatible with the image (matching arch/libc) — a static
// build runs anywhere; documented in docs/backends.md.
func (p *dockerProvisioner) copyAfBinary() error {
	if p.payloadViaBinds {
		// The bind mounts in `docker run` already serve the binary read-only
		// at dockerAfBinaryPath — nothing to copy.
		return nil
	}
	source := p.stagedAfPath
	if source == "" {
		source = p.afBin
	}
	out, err := p.docker(dockerShortStepTimeout, "cp", source, p.containerID+":"+dockerAfBinaryPath)
	if err != nil {
		return fmt.Errorf("backend=docker: copying the af binary into the container failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if p.stagedAfPath != "" {
		// The staged copy carries 0755 and cp preserves mode — no chmod.
		return nil
	}
	if out, err := p.execSh(dockerShortStepTimeout, "chmod +x "+shellQuote(dockerAfBinaryPath)); err != nil {
		// execSh runs as --user 0:0 unconditionally (docker cp lands the
		// binary root-owned, so only root can chmod it), so the failure is a
		// root exec regardless of session kind. It is given its own class
		// (dockerOpChmod) rather than dockerOpRootExec because its single
		// write root is /usr/local/bin/af — not the home/workspace/account
		// paths the prepare steps' root execs write — so a chmod failure's
		// capacity verdict may only be borrowed by a bind covering the
		// binary, never by an unrelated bind at the home or workspace
		// (#5424 review).
		return dockerStepErr(fmt.Errorf("backend=docker: chmod on the copied af binary failed: %s: %w", strings.TrimSpace(string(out)), err), dockerOpChmod)
	}
	return nil
}

// dockerPolicyPath is where the daemon-resolved operator policy lands in the
// container (#5571 W2.5). /etc rather than /tmp: the cp fallback's mount-table
// check refuses any non-rootfs mount covering a write target — a private
// --tmpfs is indistinguishable from a host tmpfs bind there — and --tmpfs /tmp
// is a common hardening flag, while a mount over /etc breaks the container's
// own resolv.conf/hostname/hosts and is correspondingly rare. /etc also exists
// on every image that can host a container at all.
const dockerPolicyPath = "/etc/af-policy.json"

// copyPolicy places spec.AgentPolicy in the container for the agent-server to
// read instead of the image's own config. On the cp fallback path the staged
// copy's 0644 mode is preserved by `docker cp`, leaving it world-readable for a
// non-root agent uid exactly as the old chmod made it; when staging failed the
// pre-#7577 path runs whole — a fresh temp file, cp, and the chmod. The file
// holds hook commands and program names, no credential, and the container is
// the session's own.
func (p *dockerProvisioner) copyPolicy() error {
	if len(p.spec.AgentPolicy) == 0 || p.payloadViaBinds {
		return nil
	}
	if p.stagedPolicyPath != "" {
		out, err := p.docker(dockerShortStepTimeout, "cp", p.stagedPolicyPath, p.containerID+":"+dockerPolicyPath)
		if err != nil {
			return fmt.Errorf("backend=docker: copying the operator policy into the container failed: %s: %w", strings.TrimSpace(string(out)), err)
		}
		return nil
	}
	tmp, err := os.CreateTemp("", "af-policy-*.json")
	if err != nil {
		return dockerStepErr(fmt.Errorf("backend=docker: staging the operator policy failed: %w", err), dockerOpLocal)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(p.spec.AgentPolicy); err != nil {
		_ = tmp.Close()
		return dockerStepErr(fmt.Errorf("backend=docker: staging the operator policy failed: %w", err), dockerOpLocal)
	}
	if err := tmp.Close(); err != nil {
		return dockerStepErr(fmt.Errorf("backend=docker: staging the operator policy failed: %w", err), dockerOpLocal)
	}
	if out, err := p.docker(dockerShortStepTimeout, "cp", tmp.Name(), p.containerID+":"+dockerPolicyPath); err != nil {
		return fmt.Errorf("backend=docker: copying the operator policy into the container failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if out, err := p.execSh(dockerShortStepTimeout, "chmod 644 "+shellQuote(dockerPolicyPath)); err != nil {
		return dockerStepErr(fmt.Errorf("backend=docker: chmod on the operator policy failed: %s: %w", strings.TrimSpace(string(out)), err), dockerOpChmod)
	}
	return nil
}
