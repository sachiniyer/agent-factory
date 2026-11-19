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
		// No staged copy exists — cp the daemon's own binary through a 0755
		// host temp file so `docker cp` carries an exec'able mode verbatim.
		// An in-container chmod here would exec an image program as root
		// before the verifier cleared it (#7607 review).
		data, err := os.ReadFile(p.afBin)
		if err != nil {
			return dockerStepErr(fmt.Errorf("backend=docker: cannot read the af binary %s for container delivery: %w", p.afBin, err), dockerOpLocal)
		}
		tmp, err := stageModeCopy(data, 0o755)
		if err != nil {
			return dockerStepErr(err, dockerOpLocal)
		}
		defer os.Remove(tmp)
		source = tmp
	}
	out, err := p.docker(dockerShortStepTimeout, "cp", source, p.containerID+":"+dockerAfBinaryPath)
	if err != nil {
		return fmt.Errorf("backend=docker: copying the af binary into the container failed: %s: %w", strings.TrimSpace(string(out)), err)
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
	tmp, err := stageModeCopy(p.spec.AgentPolicy, 0o644)
	if err != nil {
		return dockerStepErr(err, dockerOpLocal)
	}
	defer os.Remove(tmp)
	if out, err := p.docker(dockerShortStepTimeout, "cp", tmp, p.containerID+":"+dockerPolicyPath); err != nil {
		return fmt.Errorf("backend=docker: copying the operator policy into the container failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// stageModeCopy writes data to a temp file under an exact mode and returns
// its path: `docker cp` preserves the source mode, so the in-container file
// lands at mode without an in-container chmod — which on the cp path would
// be an unverified image program exec'd as root (#7607 review). The caller
// removes the temp file.
func stageModeCopy(data []byte, mode os.FileMode) (string, error) {
	tmp, err := os.CreateTemp("", "af-payload-*")
	if err != nil {
		return "", fmt.Errorf("backend=docker: staging the container payload failed: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("backend=docker: staging the container payload failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("backend=docker: staging the container payload failed: %w", err)
	}
	// CreateTemp applies umask — pin the mode the delivery contract means.
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("backend=docker: staging the container payload failed: %w", err)
	}
	return tmp.Name(), nil
}
