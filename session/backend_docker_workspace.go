package session

import (
	"fmt"
	"os"
	"strings"
)

// trustWorkspaceDirectories marks every directory safe in the CONTAINER's own
// global git config, so the agent-server's worktree work over a possibly
// foreign-owned bind mount does not trip on "dubious ownership". The config
// lives in the container's home, not the operator's, so W2.7's rule about the
// host's gitconfig does not apply; it runs before the clone, as it always did.
func (p *dockerProvisioner) trustWorkspaceDirectories() error {
	out, err := p.execSessionSh(dockerShortStepTimeout, `git config --global --add safe.directory "*"`)
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
	script := gitCloneCommand(p.spec.CloneURL, dockerWorkspaceDir)
	out, err := p.execSessionSh(dockerProvisionStepTimeout, script)
	if err != nil {
		return fmt.Errorf("backend=docker: cloning %q into the container failed (is git in the image, and the URL reachable from inside the container?): %s: %w",
			p.spec.CloneURL, strings.TrimSpace(string(out)), err)
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

// copyAfBinary docker cp's the daemon's own `af` binary into the container and
// makes it executable, so `af agent-server` is runnable inside. The binary must
// be compatible with the image (matching arch/libc) — a static build runs
// anywhere; documented in docs/backends.md.
func (p *dockerProvisioner) copyAfBinary() error {
	out, err := p.docker(dockerShortStepTimeout, "cp", p.afBin, p.containerID+":"+dockerAfBinaryPath)
	if err != nil {
		return fmt.Errorf("backend=docker: copying the af binary into the container failed: %s: %w", strings.TrimSpace(string(out)), err)
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
// container (#5571 W2.5).
const dockerPolicyPath = "/tmp/af-policy.json"

// copyPolicy places spec.AgentPolicy in the container for the agent-server to
// read instead of the image's own config. docker cp lands it root-owned, and a
// BYO image may run the agent as another uid, so it is made world-readable: the
// file holds hook commands and program names, no credential, and the container
// is the session's own.
func (p *dockerProvisioner) copyPolicy() error {
	if len(p.spec.AgentPolicy) == 0 {
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
