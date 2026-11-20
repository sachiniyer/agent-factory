// The docker engine endpoint layer: which engine this environment's docker
// CLI will talk to, whether that engine is local, and when two spellings of
// an endpoint name the same engine.
//
// resolveDockerEngineEndpoint mirrors the CLI's own selection order
// (DOCKER_HOST, then DOCKER_CONTEXT, then the context chosen with
// `docker context use`, then the default socket), and
// resolveConflictingDockerSelectors refuses the one pair Docker's own
// sources disagree on, so af never guesses which engine a session lands on.
// The resolved endpoint is then classified two ways on purpose:
// localDockerEndpoint answers reachability — can this daemon dial back the
// agent-server port the engine publishes on loopback — while
// dockerEndpointOnThisHost answers identity — does a bind mount resolve on
// this host's filesystem. A tcp:// loopback endpoint satisfies the first but
// not the second: `ssh -L` makes a remote engine answer on 127.0.0.1 while
// mounting an unrelated same-named path under the wrong identity.
//
// These helpers serve the account and non-account provisioners, the
// choose-time backend picker (backend_preconditions.go), the staging sweep,
// and the orphan reaper — none of them account-scoped, which is why they
// live here rather than in backend_docker_account.go.
package session

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

const dockerEndpointFormat = "{{.Endpoints.docker.Host}}"

// rawEnvironmentValue returns the untrimmed value environ gives name, or "" when
// it is absent. The last entry wins because exec.Cmd.Env passes the last
// duplicate to the child, and the value af judges must be the one docker reads.
func rawEnvironmentValue(environ []string, name string) string {
	prefix := name + "="
	value := ""
	for _, entry := range environ {
		if strings.HasPrefix(entry, prefix) {
			value = strings.TrimPrefix(entry, prefix)
		}
	}
	return value
}

func localDockerEndpoint(endpoint string) bool {
	endpoint = strings.TrimSpace(endpoint)
	lower := strings.ToLower(endpoint)
	if strings.HasPrefix(lower, "unix://") ||
		strings.HasPrefix(lower, "npipe://") ||
		strings.HasPrefix(lower, "fd://") {
		return true
	}
	// A TCP or SSH endpoint whose host is a loopback address or the reserved
	// loopback name is local by definition — the daemon can reach it regardless
	// of transport scheme. tcp://127.0.0.1:2375, tcp://127.x.x.x:*,
	// tcp://[::1]:*, tcp://localhost:2375, tcp://localhost.:2375, and
	// ssh://user@localhost are all local; any other host (including a
	// resolvable remote hostname over TCP or an SSH target elsewhere) is
	// remote. isLoopbackHost (session/weburl.go) covers RFC 6761
	// "localhost"/"*.localhost" without DNS and ip.IsLoopback() for the full
	// 127.0.0.0/8 and ::1 ranges.
	if strings.HasPrefix(lower, "tcp://") || strings.HasPrefix(lower, "ssh://") {
		return loopbackHostedEndpoint(endpoint)
	}
	return false
}

// loopbackHostedEndpoint reports whether endpoint parses as a URL whose host is
// loopback. u.Hostname() strips the port, userinfo, and IPv6 brackets, so
// ssh://user@127.0.0.1 answers on 127.0.0.1, not on "user@127.0.0.1".
func loopbackHostedEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

// dockerEndpointOnThisHost is the stricter locality proof that bind-mount
// identity and SELinux labelling need. It answers "is the Docker DAEMON on this
// host", not "can this host reach the endpoint" — a different question, because
// Docker resolves and labels a bind source on the daemon host.
//
// A socket scheme (unix://, npipe://, fd://) is the engine's own transport on
// this machine, and an ssh:// endpoint aimed at loopback terminates on this
// host's sshd, so the daemon it reaches is local too. A tcp:// endpoint does
// NOT get this proof even on loopback: `ssh -L 2375:host:2375` — Docker's
// documented remote-access transport — makes a remote daemon answer on
// 127.0.0.1 while resolving bind sources on the remote host, where an
// unrelated same-named path would mount under the wrong identity. Reachability
// is enough for a non-account session's dial-back; it is not evidence of which
// host a bind mount lands on.
func dockerEndpointOnThisHost(endpoint string) bool {
	endpoint = strings.TrimSpace(endpoint)
	lower := strings.ToLower(endpoint)
	if strings.HasPrefix(lower, "unix://") ||
		strings.HasPrefix(lower, "npipe://") ||
		strings.HasPrefix(lower, "fd://") {
		return true
	}
	if strings.HasPrefix(lower, "ssh://") {
		return loopbackHostedEndpoint(endpoint)
	}
	return false
}

// remoteDockerEngineError is the one wording for "the Docker engine is not on this
// host", shared by Provision (create time) and BackendUnusableReason (choose time)
// — mirroring dockerCLIMissingError / missingOriginError, so the reason the web
// gives at choose time is the reason the CLI prints at create time. The published
// agent-server port is on the engine host's loopback; the daemon dials its own, so
// a remote engine is unreachable and the failure would otherwise surface later as
// an opaque `dial tcp 127.0.0.1:<port>: connect: connection refused`.
func remoteDockerEngineError(endpoint string) error {
	return fmt.Errorf("backend=docker: the docker backend requires a local Docker engine, but the Docker endpoint %q is remote; the in-container agent-server port is published on the engine host's loopback, which this daemon cannot reach", endpoint)
}

// dockerLocalityProbeError is the fail-closed wording for "I could not establish
// whether the Docker engine is local", shared by Provision and
// BackendUnusableReason. The account path names the account inline; this is the
// account-agnostic half, matching that path's posture (refuse, do not proceed)
// when locality cannot be proven.
func dockerLocalityProbeError(err error) error {
	return fmt.Errorf("backend=docker: cannot establish that the Docker daemon is local: %w", err)
}

// resolveDockerEngineEndpoint resolves the Docker endpoint this daemon's docker
// CLI will talk to and reports whether it is REACHABLE-local: localDockerEndpoint
// is the dial-back classification, shared by the provision-time
// dockerEngineEndpoint method and the choose-time picker locality check so
// create-time and choose-time cannot disagree on what "local" means for
// reachability. Host-identity consumers (ensureAccountDockerEngineLocal and
// bindMountRelabel) resolve the endpoint through this same call but apply the
// stricter dockerEndpointOnThisHost to it instead — see its comment for why a
// loopback TCP endpoint is not host-identity proof.
//
// It reads DOCKER_HOST and DOCKER_CONTEXT from environ exactly as the docker CLI
// receives them. The CLI picks its engine in this order (resolveContextName in
// docker/cli; the order is the same in 19.03 through 29.x, and since 23.0 an
// empty value counts as unset): a non-empty DOCKER_HOST, then a non-empty
// DOCKER_CONTEXT, then the context chosen with `docker context use`, then the
// default socket. With DOCKER_HOST alone, af uses that value directly.
// With neither set, `docker context inspect` reports the CLI's own choice. When
// both are set, the CLI's code and its reference disagree about which one wins,
// so resolveConflictingDockerSelectors refuses unless both name the same
// endpoint.
//
// ensureAccountDockerEngineLocal and bindMountRelabel make this same call. Two
// concerns turn on the engine being local: Docker resolves AND labels a bind
// source on the daemon host (not the CLI host), and the agent-server port
// publishes on the engine host's loopback while the daemon dials its own. A
// remote engine breaks the latter for every docker session, not only
// account-scoped ones.
func resolveDockerEngineEndpoint(environ []string) (endpoint string, local bool, err error) {
	// The CLI tests both variables with os.Getenv(...) != "", so a
	// whitespace-only value still counts as set. Presence is decided on the raw
	// value for that reason.
	rawHost := rawEnvironmentValue(environ, "DOCKER_HOST")
	rawContext := rawEnvironmentValue(environ, "DOCKER_CONTEXT")
	ctx, cancel := context.WithTimeout(context.Background(), dockerShortStepTimeout)
	defer cancel()
	switch {
	case rawHost != "" && rawContext != "":
		endpoint, err = resolveConflictingDockerSelectors(ctx, environ, rawContext)
		if err != nil {
			return "", false, err
		}
	case strings.TrimSpace(rawHost) != "":
		endpoint = strings.TrimSpace(rawHost)
	default:
		// A whitespace-only DOCKER_HOST lands here too. The CLI still selects the
		// default context for it, and the inspect below reports that choice.
		out, derr := dockerExec(ctx, environ, "context", "inspect", "--format", dockerEndpointFormat)
		if derr != nil {
			return "", false, fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), derr)
		}
		endpoint = strings.TrimSpace(string(out))
	}
	return endpoint, localDockerEndpoint(endpoint), nil
}

// resolveConflictingDockerSelectors handles DOCKER_HOST and DOCKER_CONTEXT set
// together, the one pair Docker's own sources disagree on (#4413). Every CLI
// release dials DOCKER_HOST and ignores DOCKER_CONTEXT. The CLI reference
// documents DOCKER_CONTEXT as overriding DOCKER_HOST. A user who set both may
// have meant either one, and a wrong guess puts the session on an engine they
// did not choose. af therefore has the CLI resolve both, and proceeds only when
// they name the same endpoint.
//
// "default" is the context a set DOCKER_HOST selects. Resolving it through the
// CLI keeps Docker's own normalization of the value: a whitespace-only
// DOCKER_HOST becomes the default socket, not an empty endpoint. The `--` stops
// a context name that starts with a dash from being read as a flag.
func resolveConflictingDockerSelectors(ctx context.Context, environ []string, rawContext string) (string, error) {
	out, err := dockerExec(ctx, environ, "context", "inspect", "--format", dockerEndpointFormat, "--", "default", rawContext)
	if err != nil {
		return "", fmt.Errorf("DOCKER_HOST and DOCKER_CONTEXT are both set, and DOCKER_CONTEXT=%q could not be resolved to compare them (%s: %w) — unset the one you did not mean",
			rawContext, strings.TrimSpace(string(out)), err)
	}
	endpoints := strings.Split(strings.TrimRight(string(out), "\r\n"), "\n")
	if len(endpoints) != 2 {
		return "", fmt.Errorf("DOCKER_HOST and DOCKER_CONTEXT are both set, and `docker context inspect` reported %d endpoints instead of 2 when comparing them — unset the one you did not mean",
			len(endpoints))
	}
	hostEndpoint := strings.TrimSpace(endpoints[0])
	contextEndpoint := strings.TrimSpace(endpoints[1])
	if !sameDockerEndpoint(hostEndpoint, contextEndpoint) {
		return "", fmt.Errorf("DOCKER_HOST and DOCKER_CONTEXT are both set and select different Docker engines (DOCKER_HOST resolves to %q, DOCKER_CONTEXT=%q to %q); the docker CLI dials DOCKER_HOST but its reference documents DOCKER_CONTEXT as the override, so af will not guess — unset the one you did not mean",
			hostEndpoint, rawContext, contextEndpoint)
	}
	return hostEndpoint, nil
}

// sameDockerEndpoint reports whether two endpoint strings provably name the
// same engine. Byte-equal strings always do — a context storing the literal
// DOCKER_HOST value needs no further proof. Otherwise both are canonicalized
// and compared, so differently SERIALIZED spellings of one endpoint (case,
// default port, trailing dot or slash) still agree: refusing those would be a
// fail-closed guard rejecting a legitimate configuration (#4413 review).
//
// When an endpoint cannot be canonicalized at all the comparison reports
// false rather than leaning either way: af could not prove the selectors
// agree, and unprovable agreement is exactly the ambiguity this guard exists
// to refuse. The same conclusion follows when both are unparseable but
// byte-different.
func sameDockerEndpoint(a, b string) bool {
	if a == b {
		return true
	}
	canonicalA, okA := canonicalDockerEndpoint(a)
	canonicalB, okB := canonicalDockerEndpoint(b)
	return okA && okB && canonicalA == canonicalB
}

// canonicalDockerEndpoint normalizes an endpoint string for the same-engine
// comparison and reports whether the normalization applied. It covers only
// spelling, never identity:
//
//   - scheme and host letter case (`TCP://LOCALHOST:2375`),
//   - tcp's default port (`tcp://x` is `tcp://x:2375`) — but NOT ssh's: Docker's
//     ssh connhelper passes `-p` only for an explicit URL port and otherwise
//     lets OpenSSH config supply one, so `ssh://h` and `ssh://h:22` can reach
//     different daemons when ~/.ssh/config sets Port on an alias (#4413 review).
//   - one trailing dot on a hostname (`localhost.` is `localhost`),
//   - IPv6 in any spelling netip accepts (`[0:0:0:0:0:0:0:1]` is `::1`),
//   - a trailing path slash, and the unix:// split url.Parse makes between
//     host and path (`unix://var/run/docker.sock` joins to the same socket as
//     `unix:///var/run/docker.sock`).
//
// It deliberately does NOT resolve names or equate addresses: tcp://localhost
// and tcp://127.0.0.1 likely reach one daemon, but proving that needs DNS —
// and DNS answers in a refusal guard are a fail-open hazard. They still
// compare different. url.Parse has already rejected non-numeric ports, so a
// port that survives here is a number Docker will dial.
func canonicalDockerEndpoint(endpoint string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Scheme == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "unix", "npipe", "fd":
		// The socket is host+path rejoined: url.Parse puts the first segment
		// into Host for unix://sock/..., leaving Path to carry the rest. No
		// case folding or dot-segment cleaning — the path is what docker opens.
		socket := u.Host + u.Path
		if scheme == "unix" && !strings.HasPrefix(socket, "/") {
			socket = "/" + socket
		}
		return scheme + "://" + socket, true
	case "tcp", "ssh":
		host := u.Hostname()
		if host == "" {
			return "", false
		}
		if addr, aerr := netip.ParseAddr(host); aerr == nil {
			host = addr.String()
		} else {
			host = strings.TrimSuffix(strings.ToLower(host), ".")
		}
		port := u.Port()
		if port == "" && scheme == "tcp" {
			// tcp's default is fixed in docker/cli; ssh has no safe default —
			// an omitted port there defers to OpenSSH config, which this
			// function cannot see, so the canonical form keeps it absent.
			port = "2375"
		}
		var b strings.Builder
		b.WriteString(scheme)
		b.WriteString("://")
		if u.User != nil {
			b.WriteString(u.User.String())
			b.WriteString("@")
		}
		b.WriteString(host)
		if port != "" {
			b.WriteString(":")
			b.WriteString(port)
		}
		if p := strings.TrimSuffix(u.Path, "/"); p != "" {
			b.WriteString(p)
		}
		if u.ForceQuery || u.RawQuery != "" {
			b.WriteString("?")
			b.WriteString(u.RawQuery)
		}
		if u.Fragment != "" {
			b.WriteString("#")
			b.WriteString(u.Fragment)
		}
		return b.String(), true
	default:
		return "", false
	}
}
