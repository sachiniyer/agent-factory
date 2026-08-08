package session

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

const dockerEndpointFormat = "{{.Endpoints.docker.Host}}"

func accountDockerDeniedNames(agent string) map[string]struct{} {
	denied := make(map[string]struct{})
	for _, name := range sessionenv.AccountIdentityNames(agent) {
		denied[name] = struct{}{}
	}
	for _, name := range sessionenv.AgentAuthSelectors(agent) {
		denied[name] = struct{}{}
	}
	return denied
}

func filterAccountPassthrough(names []string, agent string) []string {
	denied := accountDockerDeniedNames(agent)
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, drop := denied[name]; !drop {
			out = append(out, name)
		}
	}
	return out
}

// validateAccountDockerRunArgs refuses repo-controlled sources that Docker can
// apply after af has selected the account. Environment files are opaque to af,
// so none are accepted in account mode; direct environment options are refused
// only when they can choose another identity or authentication mode.
func validateAccountDockerRunArgs(args []string, agent string) error {
	denied := accountDockerDeniedNames(agent)
	checkEnv := func(value string) error {
		name, _, _ := strings.Cut(value, "=")
		if _, refused := denied[name]; refused {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot set %s for an account-scoped %s session because it can override the selected identity",
				name, agent)
		}
		return nil
	}
	checkMount := func(value string) error {
		protectedPath := func(target string) string {
			target = path.Clean(target)
			for _, protected := range []string{dockerAccountHome, dockerAccountRuntimeHome} {
				if target == protected || strings.HasPrefix(target, protected+"/") {
					return protected
				}
			}
			return ""
		}
		mountsProtectedPath := func() string {
			for _, field := range strings.Split(value, ",") {
				key, target, ok := strings.Cut(field, "=")
				if !ok || (key != "dst" && key != "destination" && key != "target") {
					continue
				}
				if protected := protectedPath(target); protected != "" {
					return protected
				}
			}
			// --volume and --tmpfs use ':' between fields. Looking for an
			// exact field avoids refusing harmless paths such as
			// /af-account-cache.
			for _, field := range strings.Split(value, ":") {
				if protected := protectedPath(field); protected != "" {
					return protected
				}
			}
			return ""
		}
		if protected := mountsProtectedPath(); protected != "" {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot install an account mount over %s because it would replace af's selected account boundary",
				protected)
		}
		return nil
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--env-file" || arg == "-env-file":
			return fmt.Errorf("backend=docker: docker.run_args cannot use %s for an account-scoped session because af cannot prove the file contains no competing identity", arg)
		case strings.HasPrefix(arg, "--env-file="):
			return fmt.Errorf("backend=docker: docker.run_args cannot use --env-file for an account-scoped session because af cannot prove the file contains no competing identity")
		case arg == "-e" || arg == "--env":
			if index+1 < len(args) {
				index++
				if err := checkEnv(args[index]); err != nil {
					return err
				}
			}
		case strings.HasPrefix(arg, "--env="):
			if err := checkEnv(strings.TrimPrefix(arg, "--env=")); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-e="):
			if err := checkEnv(strings.TrimPrefix(arg, "-e=")); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-e") && len(arg) > 2:
			if err := checkEnv(strings.TrimPrefix(arg, "-e")); err != nil {
				return err
			}
		case arg == "-v" || arg == "--volume" || arg == "--mount" || arg == "--tmpfs":
			if index+1 < len(args) {
				index++
				if err := checkMount(args[index]); err != nil {
					return err
				}
			}
		case strings.HasPrefix(arg, "--volume=") || strings.HasPrefix(arg, "--mount=") || strings.HasPrefix(arg, "--tmpfs="):
			if err := checkMount(strings.SplitN(arg, "=", 2)[1]); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-v") && len(arg) > 2:
			if err := checkMount(strings.TrimPrefix(arg, "-v")); err != nil {
				return err
			}
		}
	}
	return nil
}

func environmentValue(environ []string, name string) string {
	prefix := name + "="
	for _, entry := range environ {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(entry, prefix))
		}
	}
	return ""
}

func localDockerEndpoint(endpoint string) bool {
	endpoint = strings.ToLower(strings.TrimSpace(endpoint))
	return strings.HasPrefix(endpoint, "unix://") ||
		strings.HasPrefix(endpoint, "npipe://") ||
		strings.HasPrefix(endpoint, "fd://")
}

// ensureAccountDockerEngineLocal proves bind mounts are interpreted on this
// host. Docker resolves a bind source on the daemon host, not the CLI host, so a
// remote endpoint would mount an unrelated path while reporting the local
// account name.
func (p *dockerProvisioner) ensureAccountDockerEngineLocal() error {
	environ := p.dockerEnvironment()
	dockerHost := environmentValue(environ, "DOCKER_HOST")
	dockerContext := environmentValue(environ, "DOCKER_CONTEXT")
	endpoint := dockerHost
	if dockerContext != "" || endpoint == "" {
		out, err := p.docker(dockerShortStepTimeout, "context", "inspect", "--format", dockerEndpointFormat)
		if err != nil {
			return fmt.Errorf("backend=docker: cannot establish that the Docker daemon is local before mounting account %q: %s: %w",
				p.spec.Account.Name, strings.TrimSpace(string(out)), err)
		}
		endpoint = strings.TrimSpace(string(out))
	}
	if !localDockerEndpoint(endpoint) {
		return fmt.Errorf(
			"backend=docker: account %q cannot be used with remote Docker endpoint %q: bind mounts resolve on the daemon host, so this host's account path would not be the selected identity",
			p.spec.Account.Name, endpoint)
	}
	return nil
}

func parseContainerOwner(out string) (string, error) {
	owner := strings.TrimSpace(out)
	uid, gid, ok := strings.Cut(owner, ":")
	if !ok || uid == "" || gid == "" {
		return "", fmt.Errorf("expected numeric uid:gid, got %q", owner)
	}
	if _, err := strconv.ParseUint(uid, 10, 32); err != nil {
		return "", fmt.Errorf("invalid uid in %q: %w", owner, err)
	}
	if _, err := strconv.ParseUint(gid, 10, 32); err != nil {
		return "", fmt.Errorf("invalid gid in %q: %w", owner, err)
	}
	return owner, nil
}

// prepareAccountUser asks the mounted directory who owns it in the container's
// user namespace, then prepares a writable HOME/workspace for that same uid.
// Rootful Docker reports the host af uid; rootless Docker commonly reports 0,
// which maps back to the invoking host user. Either way writes through the bind
// mount retain the ownership local account sessions need.
func (p *dockerProvisioner) prepareAccountUser() error {
	if p.spec.Account.Dir == "" {
		return nil
	}
	out, err := p.docker(dockerShortStepTimeout,
		"exec", "--user", "0:0", p.containerID, "stat", "-c", "%u:%g", dockerAccountHome)
	if err != nil {
		return fmt.Errorf("backend=docker: cannot identify the mounted account owner: %s: %w", strings.TrimSpace(string(out)), err)
	}
	owner, err := parseContainerOwner(string(out))
	if err != nil {
		return fmt.Errorf("backend=docker: cannot identify the mounted account owner: %w", err)
	}
	script := fmt.Sprintf("mkdir -p %s %s && chown %s %s %s",
		shellQuote(dockerAccountRuntimeHome), shellQuote(dockerWorkspaceDir), owner,
		shellQuote(dockerAccountRuntimeHome), shellQuote(dockerWorkspaceDir))
	out, err = p.docker(dockerShortStepTimeout,
		"exec", "--user", "0:0", p.containerID, "sh", "-c", script)
	if err != nil {
		return fmt.Errorf("backend=docker: cannot prepare the account-owned container home and workspace: %s: %w", strings.TrimSpace(string(out)), err)
	}
	p.containerUser = owner
	return nil
}

func (p *dockerProvisioner) sessionHome() string {
	if p.spec.Account.Dir != "" {
		return dockerAccountRuntimeHome
	}
	return dockerContainerHome
}

func (p *dockerProvisioner) sessionExecOptions() []string {
	if p.containerUser == "" {
		return nil
	}
	return []string{"--user", p.containerUser, "-e", "HOME=" + p.sessionHome()}
}

func (p *dockerProvisioner) execSessionSh(timeout time.Duration, script string) ([]byte, error) {
	args := []string{"exec"}
	args = append(args, p.sessionExecOptions()...)
	args = append(args, p.containerID, "sh", "-c", script)
	return p.docker(timeout, args...)
}
