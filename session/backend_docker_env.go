// The docker provisioner's invocation half: how it names its agent, filters
// the client environment, pins the engine selector, and shells `docker` —
// the seam every provision-time call (info/run/exec/cp/port) passes through.
// Creation lives in backend_docker.go; destruction in backend_docker_reap.go.
package session

import (
	"context"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// docker runs `docker <args...>` with a timeout and returns its combined
// output, under the provision's pinned DOCKER_CONTEXT when one was resolved —
// absent on paths that never provisioned (reap, teardown), which re-resolve
// the current context as they always did.
func (p *dockerProvisioner) docker(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return dockerExec(ctx, p.dockerEnvironment(), args...)
}

func (p *dockerProvisioner) agentName() string {
	// The grant predicate, not the identity classifier: the credential mounts
	// and run_args validation this feeds install operator material, so the
	// agent name must be one af can attribute to the operator. The remote
	// variant is the honest one here — the executable resolves on the image's
	// filesystem, which the host cannot stat; the container's clone lives at
	// dockerWorkspaceDir, so an absolute program path inside it resolves to
	// repository content and earns no grant (#5265).
	agent := sessionenv.GrantAgentForRemoteCommand(p.program, dockerWorkspaceDir)
	if agent == "" && strings.TrimSpace(p.program) == "" {
		return tmux.ProgramClaude
	}
	return agent
}

func (p *dockerProvisioner) dockerEnvironment() []string {
	env := sessionenv.DockerCLIEnvironment(os.Environ(), p.agentName(), p.spec.SessionEnvPassthrough)
	// The provision resolved the operator's selector once (Codex on #7616):
	// when neither DOCKER_HOST nor DOCKER_CONTEXT named one, the env carries
	// none, and every call would re-resolve the CURRENT context — a
	// `docker context use` landing between the engine-ID read and `docker
	// run` would create on an engine the marker never recorded. Materializing
	// the resolved context name pins every later call to the engine the
	// provision chose. A restored reaper never sets it, so the engine-verify
	// on the reap path still reads the operator's live selector.
	if p.dockerContextPin != "" {
		env = append(env, "DOCKER_CONTEXT="+p.dockerContextPin)
	}
	// The bare `-e NAME` emitted for a normalized proxy twin resolves here:
	// synthesize NAME=<forwarded spelling's host value>. The twin is not in the
	// client's allowlist unless the operator forwarded it, so the synthesized
	// entry is the only one that name can resolve to. Same credential-mount
	// scope as containerEnvironmentNames (#7529 R8).
	if len(p.credentialMounts) == 0 {
		return env
	}
	_, synth := credentialMountGroupForwarding(p.spec.SessionEnvPassthrough, os.Environ())
	return append(env, synth...)
}

// execSh runs a `sh -c <script>` inside the container as root (0:0). It is the
// provisioning step that operates on root-owned container state — `docker cp`
// lands the `af` binary owned by root, and `chmod +x` on it must run as root
// regardless of the image's default USER. A non-root default USER (a common
// hardening practice on BYO images) cannot chmod a root-owned file, so gating
// the root override on account sessions left the non-account chmod failing with
// "Operation not permitted" on any such image. Running as 0:0 unconditionally
// is a no-op for a root-default image (its default user IS root) and is the
// only thing that works for a non-root one.
func (p *dockerProvisioner) execSh(timeout time.Duration, script string) ([]byte, error) {
	args := []string{"exec", "--user", "0:0", p.containerID, dockerContainerShell, "-c", p.trustPathUnpinnedScript(script)}
	return p.docker(timeout, args...)
}

// trustPathUnpinnedScript prefixes a provisioning script with an unset of
// the path-valued trust names THIS session pins defined-and-empty in the
// container environment (#7889). The pin must stand there — `docker run`
// cannot unset an image ENV — but a defined-empty SSL_CERT_FILE or
// SSL_CERT_DIR is a PATH an OpenSSL-family TLS client loads as zero roots
// (the five CA-bundle spellings keep their pin: their measured empty read
// is falsy-safe). Unsetting first makes every env-honoring stack a
// provisioning child might carry fall back to its compiled platform store
// instead of reading the pin's empty path. Only PINNED names are unset —
// an operator-forwarded SSL_CERT_FILE is the session's working CA config
// and the clone needs it — and a pinned slot beside a FORWARDED sibling
// keeps defined-empty rather than unsetting: SSL_CERT_FILE/SSL_CERT_DIR
// are independent slots, so dropping the pin to unset would restore the
// platform default and add the image's roots to a trust the operator
// bounded to their forwarded value (the sibling guard the exec shim and
// the agent-server startup apply too, #7889). No .env-fill channel exists
// on these children, so unlike the agent's exec shim — which must keep
// the name claimed — unset is the inert form here.
func (p *dockerProvisioner) trustPathUnpinnedScript(script string) string {
	var unset []string
	for _, name := range sessionenv.TrustPathPinNames() {
		if !slices.Contains(p.credentialEnvPins, name) {
			continue
		}
		// `[ -n "$SIBLING" ] ||` keeps the pin when the sibling carries a
		// (forwarded) value — defined-empty is the only spelling that
		// contributes no roots for OpenSSL and openssl-probe alike.
		switch name {
		case "SSL_CERT_FILE":
			unset = append(unset, `[ -n "$SSL_CERT_DIR" ] || unset SSL_CERT_FILE`)
		case "SSL_CERT_DIR":
			unset = append(unset, `[ -n "$SSL_CERT_FILE" ] || unset SSL_CERT_DIR`)
		default:
			unset = append(unset, "unset "+name)
		}
	}
	if len(unset) == 0 {
		return script
	}
	return strings.Join(unset, "; ") + "; " + script
}

func (p *dockerProvisioner) shortID() string {
	if len(p.containerID) > 12 {
		return p.containerID[:12]
	}
	return p.containerID
}
