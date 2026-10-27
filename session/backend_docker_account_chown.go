package session

import (
	"fmt"
	"strings"
)

// credentialMountTargets returns the in-container destinations of the
// read-only agent-credential bind mounts (docker.mount_agent_credentials,
// #2194) that fall under home — the single files agentCredentialMounts
// bind-mounts at dockerContainerHome/<rel>. Each `-v` entry of mounts is the
// pair ("-v", "host:container:mode"); host and container are absolute unix
// paths with no colons, so the container destination is the segment between
// the last two colons. Only strict descendants of home are returned: the
// credential always mounts under the non-account session home, and pruning
// home itself would skip the whole chown.
func credentialMountTargets(mounts []string, home string) []string {
	var targets []string
	for i := 0; i+1 < len(mounts); i++ {
		if mounts[i] != "-v" {
			continue
		}
		spec := mounts[i+1]
		last := strings.LastIndexByte(spec, ':')
		if last < 0 {
			continue
		}
		body := spec[:last]
		sep := strings.LastIndexByte(body, ':')
		if sep < 0 {
			continue
		}
		container := body[sep+1:]
		if container == "" || home == "" {
			continue
		}
		if strings.HasPrefix(container, home+"/") {
			targets = append(targets, container)
		}
	}
	return targets
}

// chownRecursiveScript returns a shell snippet that chowns every entry under
// home to owner, recursively, except the read-only credential bind mounts in
// prune — a bare `chown -R` would descend into one and fail with EROFS,
// aborting provisioning before the agent can read its credential (#5424
// review). With no prune targets `chown -R` is simpler and stays. With prune
// targets, `find -prune` skips each mounted file (and its subtree, though
// they are single files) and chowns everything else under home, including
// home itself, so root-owned image descendants such as /root/.codex or
// /root/.config become readable and writable by the selected uid. The
// mounted credential file's own ownership is already correct on the host, so
// skipping it loses nothing.
func chownRecursiveScript(home, owner string, prune []string) string {
	if len(prune) == 0 {
		return fmt.Sprintf("chown -R %s %s", owner, shellQuote(home))
	}
	var b strings.Builder
	b.WriteString("find ")
	b.WriteString(shellQuote(home))
	b.WriteString(" \\( ")
	for i, p := range prune {
		if i > 0 {
			b.WriteString(" -o ")
		}
		b.WriteString("-path ")
		b.WriteString(shellQuote(p))
	}
	b.WriteString(" \\) -prune -o -exec chown ")
	b.WriteString(owner)
	b.WriteString(" {} +")
	return b.String()
}
