package session

import (
	"net/url"
	"path"
	"strings"
)

// dockerAgentEndpointHosts are the first-party endpoint domains each agent's
// credential-bearing traffic resolves to. A repo --add-host entry remapping
// one of these hands the agent's authenticated requests — and the credential
// they carry — to an address the repository chose, which is the #7867 R3
// routing class closed at the option surface. Domains rather than exact
// hosts because the API hosts sit under them (api.anthropic.com,
// auth.openai.com, every <region>-aiplatform.googleapis.com vertex name),
// and deliberately narrow: only the providers this roster's credential
// traffic already goes to, so ordinary --add-host entries — a database, a
// dev service, host.docker.internal — stay allowed. Agents absent from the
// table (aider: env-key auth only) contribute no static hosts and rely on
// the forwarded-value extraction below.
var dockerAgentEndpointHosts = map[string][]string{
	"claude":   {"anthropic.com", "claude.ai"},
	"codex":    {"openai.com", "chatgpt.com"},
	"gemini":   {"googleapis.com", "google.com"},
	"amp":      {"ampcode.com"},
	"opencode": {"openai.com", "anthropic.com", "googleapis.com", "openai.azure.com", "openrouter.ai", "deepseek.com", "groq.com", "mistral.ai", "cohere.com", "x.ai"},
	"devin":    {"devin.ai"},
}

// dockerSharedEndpointHosts are credential-bearing endpoint hosts shared by
// every session on a guarded path. github.com is one: the git clone/fetch and
// gh API traffic in the container carries whatever credential the session
// holds — a forwarded GH_TOKEN or gh's stored auth — and GH_HOST is already a
// refused routing name for exactly this reason (#7531). Remapping the host is
// the same route with none of the env plumbing.
var dockerSharedEndpointHosts = []string{"github.com"}

// dockerResolutionProtectedPaths are the container files name resolution
// reads. A repo mount at any of them decides where every hostname the agent
// resolves lands — --add-host's bulk form, closed for the same reason.
var dockerResolutionProtectedPaths = []string{
	"/etc/hosts",
	"/etc/resolv.conf",
	"/etc/nsswitch.conf",
}

// dockerTrustStorePaths are the container paths the agents' TLS stacks read
// their root stores from: OpenSSL's hashed dir and bundle files, the
// ca-certificates source dirs update-ca-certificates composes from, and
// p11-kit's /etc/pki tree Fedora/RHEL-family images trust through. A repo
// mount shadowing any of them makes the agent trust certificate authorities
// the repository installed — the trust-root half of the routing class the
// SSL_CERT_*/NODE_EXTRA_CA_CERTS env names already refuse.
var dockerTrustStorePaths = []string{
	"/etc/ssl/certs",
	"/etc/ssl/cert.pem",
	"/etc/ssl/ca-bundle.pem",
	"/usr/share/ca-certificates",
	"/usr/local/share/ca-certificates",
	"/etc/pki",
}

// dockerSessionEndpointHosts is the host set a repo --add-host entry may not
// remap on a credential-carrying session: the agent's first-party endpoint
// domains plus github.com, plus every host the session's effective
// environment already routes to. The env-derived half covers what the static
// table cannot: an operator's forwarded gateway (a corporate proxy in a
// forwarded HTTPS_PROXY, a forwarded OPENAI_BASE_URL or GH_HOST naming an
// enterprise deployment, a forwarded DBUS_SESSION_BUS_ADDRESS socket is a
// path not a host and simply contributes nothing), and af's own first-party
// reasserts for the late-loaded names.
func dockerSessionEndpointHosts(agent string, passthrough, environ []string) map[string]struct{} {
	hosts := make(map[string]struct{})
	for _, domain := range dockerSharedEndpointHosts {
		hosts[domain] = struct{}{}
	}
	for _, domain := range dockerAgentEndpointHosts[agent] {
		hosts[domain] = struct{}{}
	}
	values := make(map[string]string, len(environ))
	for _, entry := range environ {
		if name, value, ok := strings.Cut(entry, "="); ok {
			values[name] = value
		}
	}
	for _, name := range passthrough {
		if h := dockerEnvValueHost(values[name]); h != "" {
			hosts[h] = struct{}{}
		}
	}
	for _, value := range credentialMountFirstPartyNames[agent] {
		if h := dockerEnvValueHost(value); h != "" {
			hosts[h] = struct{}{}
		}
	}
	return hosts
}

// dockerEnvValueHost extracts the host an environment value routes to, or ""
// when the value names no host. Two shapes count: a URL whose host parses
// (https://gateway.corp:8443, unix:// is deliberately hostless), and a bare
// host or host:port (GH_HOST's form — "ghe.corp.internal"). A bare value
// qualifies only when it is hostname-shaped whole — letters, digits, dots and
// dashes with a dot — so a path, a sentence, or a flag value is never a host.
func dockerEnvValueHost(value string) string {
	if strings.Contains(value, "://") {
		if u, err := url.Parse(value); err == nil {
			return strings.ToLower(u.Hostname())
		}
		return ""
	}
	host := value
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if !strings.Contains(host, ".") {
		return ""
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '-' {
			return ""
		}
	}
	return strings.ToLower(host)
}

// dockerAddHostNames returns every hostname an --add-host value maps. Docker
// reads each comma-separated entry as host:ip (the host=ip spelling is the
// IPv6 form — "my-hostname=[2001:db8::33]" in the reference — and still
// accepted for IPv4), so the host half of either separator is what the check
// reads — "host.docker.internal:host-gateway" reports host.docker.internal
// and stays allowed. The = separator is tried first: a ':' cut would split
// inside a bracketed IPv6 address and hand the check a mangled host.
func dockerAddHostNames(value string) []string {
	var names []string
	for _, entry := range strings.Split(value, ",") {
		for _, field := range strings.Fields(entry) {
			host, _, ok := strings.Cut(field, "=")
			if !ok {
				host, _, _ = strings.Cut(field, ":")
			}
			if host != "" {
				names = append(names, host)
			}
		}
	}
	return names
}

// dockerHostProtected reports the endpoint host an --add-host hostname would
// remap: the hostname itself or any ancestor domain in the protected set —
// api.anthropic.com and anthropic.com alike. "" for anything else. The match
// lowercases first: /etc/hosts entries resolve case-insensitively (glibc's
// files-parse uses strcasecmp), so API.ANTHROPIC.COM maps the endpoint too.
func dockerHostProtected(host string, endpoints map[string]struct{}) string {
	for h := strings.ToLower(host); h != ""; {
		if _, ok := endpoints[h]; ok {
			return h
		}
		dot := strings.IndexByte(h, '.')
		if dot < 0 {
			return ""
		}
		h = h[dot+1:]
	}
	return ""
}

// dockerRoutingProtectedPath reports which resolution or trust-store path a
// container mount target lands on, inside, or above — ancestor mounts shadow
// their whole subtree, so a target at /etc or / shadows every protected path
// beneath it, and a target under a trust dir (a single forged PEM inside
// /etc/ssl/certs) is as dangerous as the dir itself. "" for anything else.
func dockerRoutingProtectedPath(target string) string {
	target = path.Clean(target)
	ancestor := func(protected string) bool {
		return target == "/" || strings.HasPrefix(protected, target+"/")
	}
	for _, protected := range dockerResolutionProtectedPaths {
		if target == protected || ancestor(protected) {
			return protected
		}
	}
	for _, protected := range dockerTrustStorePaths {
		if target == protected ||
			strings.HasPrefix(target, protected+"/") ||
			ancestor(protected) {
			return protected
		}
	}
	return ""
}

// dockerRoutingProtectedReason returns the because-clause for a refused mount
// over a protected path, worded for which mechanism the path defends.
func dockerRoutingProtectedReason(protected string) string {
	for _, p := range dockerResolutionProtectedPaths {
		if p == protected {
			return "it decides where every hostname in the container resolves — including the agent's endpoint hosts — so a repo-controlled file there routes the session's authenticated traffic the way an --add-host to an endpoint does"
		}
	}
	return "it is a TLS trust store the agent's credential-bearing requests verify against — a repo-controlled store there makes the agent trust certificate authorities the repository installed"
}

// dockerVolumeTarget reports the container path a -v/--volume value mounts
// at. The -v spec is host-src:container-dst[:opts] with the single-field form
// an anonymous volume AT the path — so the target is the second field when
// present, the only field otherwise. A field that is not a path (an option
// like "ro" in dst position makes Docker refuse the whole mount) reports
// whatever it is — dockerRoutingProtectedPath only matches real protected
// paths, so a non-path is simply unprotected.
func dockerVolumeTarget(value string) string {
	fields := strings.Split(value, ":")
	if len(fields) == 1 {
		return fields[0]
	}
	return fields[1]
}

// dockerRoutingMountTarget extracts the container path a mount option's value
// installs at, whatever the spelling: --mount's dst=/target=/destination=
// field (dockerMountFields reads Docker's own CSV), or -v/--volume's
// ':'-field target. --tmpfs is deliberately absent — its spec puts the target
// FIRST (dst[:options]), which dockerTmpfsTarget reads.
func dockerRoutingMountTarget(value string) string {
	for _, field := range dockerMountFields(value) {
		if target, ok := dockerMountTarget(field); ok {
			return target
		}
	}
	return dockerVolumeTarget(value)
}

// dockerTmpfsTarget reports the container path a --tmpfs value mounts at. The
// tmpfs spec is dst[:options] — the target is the FIRST field, where a -v
// spec puts it second — so routing a --tmpfs through dockerVolumeTarget reads
// "rw,mode=1777" as the mount point and misses the real one.
func dockerTmpfsTarget(value string) string {
	target, _, _ := strings.Cut(value, ":")
	return target
}
