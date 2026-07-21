// Package sessionenv defines the environment boundary between Agent Factory's
// control plane and the agent processes it launches.
package sessionenv

import (
	"fmt"
	"sort"
	"strings"
)

// ExecMarker is the private argv marker used when af replaces itself with a
// filtered session process. It is intentionally not a user-facing subcommand.
const ExecMarker = "__af-session-env-exec"

var commonNames = nameSet(
	// Process and terminal basics.
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "COLORTERM",
	"TERMINFO", "TERMINFO_DIRS",
	"LANG", "LANGUAGE", "TZ", "TMPDIR", "TMP", "TEMP", "PWD",
	"EDITOR", "VISUAL", "PAGER", "NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "FORCE_COLOR",
	// User configuration and keyring access. Agents keep file/keyring login state
	// beneath HOME/XDG; DBUS_SESSION_BUS_ADDRESS is needed by Linux secret stores
	// and by systemd-run when af creates a tmux server outside its daemon unit.
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
	"DBUS_SESSION_BUS_ADDRESS",
	// tmux adds TMUX_PANE itself. TMUX/TMUX_TMPDIR preserve a deliberately
	// selected non-default server when af was launched from one.
	"TMUX", "TMUX_PANE", "TMUX_TMPDIR",
	// Agent Factory state and remote-daemon selection.
	"AGENT_FACTORY_HOME", "AGENT_FACTORY_AUTO_UPDATE",
	"AF_HOME", "AF_SESSION", "AF_DAEMON_URL", "AF_DAEMON_TOKEN",
	// Git, GitHub CLI, credential helpers, commit identity, and signing agents.
	"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
	"GH_HOST", "GH_REPO", "GH_CONFIG_DIR",
	"SSH_AUTH_SOCK", "SSH_AGENT_PID", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSH_VARIANT", "GIT_ASKPASS", "SSH_ASKPASS",
	"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	"GPG_TTY", "GPG_AGENT_INFO", "GNUPGHOME",
	// Network routing and private trust roots used by agent and Git clients.
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
)

var agentNames = map[string]map[string]struct{}{
	"claude": nameSet(
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	),
	"codex": nameSet(
		"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_HOME",
		"CODEX_SQLITE_HOME", "CODEX_CA_CERTIFICATE",
	),
	"gemini": nameSet(
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "GEMINI_CLI_HOME",
		"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION",
		"GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA",
	),
	"amp": nameSet("AMP_API_KEY", "AMP_HOME"),
	"aider": nameSet(
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY",
		"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_API_VERSION",
		"AZURE_API_KEY", "AZURE_API_BASE", "AZURE_API_VERSION", "OPENAI_API_BASE", "OPENAI_BASE_URL",
		"OPENROUTER_API_KEY", "DEEPSEEK_API_KEY", "GROQ_API_KEY", "MISTRAL_API_KEY",
		"COHERE_API_KEY", "XAI_API_KEY", "AIDER_OPENAI_API_KEY", "AIDER_ANTHROPIC_API_KEY",
	),
	"opencode": nameSet(
		"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_CONFIG_CONTENT",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY",
		"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_API_VERSION",
		"OPENAI_API_BASE", "OPENAI_BASE_URL",
		"OPENROUTER_API_KEY", "DEEPSEEK_API_KEY", "GROQ_API_KEY", "MISTRAL_API_KEY",
		"COHERE_API_KEY", "XAI_API_KEY",
		"AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS",
		"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
	),
}

type conditionalNames struct {
	selector string
	names    map[string]struct{}
}

// Cloud-provider credentials are narrower than the selected agent: Claude can
// authenticate through Anthropic without needing the operator's unrelated AWS,
// Google Cloud, or Azure production credentials. Admit each provider group only
// when Claude's documented mode selector is enabled in the source environment
// or as a literal assignment on the command that launches Claude.
var conditionalAgentNames = map[string][]conditionalNames{
	"claude": {
		{
			selector: "CLAUDE_CODE_USE_BEDROCK",
			names: nameSet(
				"AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
				"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_BEARER_TOKEN_BEDROCK",
				"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
				"AWS_ROLE_SESSION_NAME", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
				"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "CLAUDE_CODE_SKIP_BEDROCK_AUTH",
				"ANTHROPIC_BEDROCK_BASE_URL",
			),
		},
		{
			selector: "CLAUDE_CODE_USE_VERTEX",
			names: nameSet(
				"ANTHROPIC_VERTEX_PROJECT_ID", "CLOUD_ML_REGION", "GOOGLE_APPLICATION_CREDENTIALS",
				"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION",
			),
		},
		{
			selector: "CLAUDE_CODE_USE_FOUNDRY",
			names: nameSet(
				"ANTHROPIC_FOUNDRY_RESOURCE", "ANTHROPIC_FOUNDRY_API_KEY",
				"AZURE_CLIENT_ID", "AZURE_TENANT_ID", "AZURE_CLIENT_SECRET", "AZURE_TOKEN_CREDENTIALS",
			),
		},
	},
}

var dockerControlNames = nameSet(
	"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH",
	"DOCKER_TLS_VERIFY", "DOCKER_API_VERSION", "DOCKER_DEFAULT_PLATFORM",
	"DOCKER_CONTENT_TRUST", "DOCKER_CONTENT_TRUST_SERVER", "BUILDKIT_HOST",
)

var dockerSSHTransportNames = nameSet("SSH_AUTH_SOCK", "SSH_AGENT_PID")

// dockerClientNames is the non-session state the short-lived Docker CLI needs
// in order to find its binary, config, credential helper, locale, temp files,
// and selected daemon. It deliberately excludes af control-plane and agent
// credentials. SSH transport remains client-only; proxy values cross only as
// explicitly authorized forward candidates, while host CA paths do not cross at
// all. Whenever session values are forwarded, repository-controlled run_args are
// refused so the image cannot request this client state as an additional
// environment.
var dockerClientNames = nameSet(
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LANGUAGE", "TZ",
	"TMPDIR", "TMP", "TEMP", "PWD",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
	"DBUS_SESSION_BUS_ADDRESS",
	// Remote Docker transport identity. Proxy/CA variables require the same
	// explicit trust grant as every value a repo-controlled --env can request.
	"SSH_AUTH_SOCK", "SSH_AGENT_PID",
)

// dockerAgentNames is a positive list because a host path is not a portable
// credential. Reusing the broader local-session list would pass CODEX_HOME,
// CLAUDE_CONFIG_DIR, certificate paths, and cloud credential-file paths into a
// container where they name unrelated or nonexistent files. New Docker agent
// credential mechanisms therefore fail closed until classified here.
var dockerAgentNames = map[string]map[string]struct{}{
	"claude": nameSet(
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"CLAUDE_CODE_OAUTH_TOKEN",
	),
	"codex": nameSet(
		"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN",
	),
}

var dockerProxyNames = nameSet(
	"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "ftp_proxy", "all_proxy", "no_proxy",
)

var dockerClientCAPathNames = nameSet(
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
)

// NormalizeExtraNames validates an explicit pass-through list, removes
// duplicates, and returns it sorted. Exact names only are accepted: allowing
// glob syntax here would turn a small escape hatch back into ambient authority.
func NormalizeExtraNames(names []string) ([]string, error) {
	set := make(map[string]struct{}, len(names))
	for idx, raw := range names {
		name := strings.TrimSpace(raw)
		if !validName(name) {
			// Do not echo raw: a common mistake is NAME=value, and rendering that
			// invalid entry would disclose the very credential this boundary protects.
			return nil, fmt.Errorf("invalid session environment variable name at position %d; use an exact POSIX name such as MY_API_KEY", idx+1)
		}
		set[name] = struct{}{}
	}
	if len(set) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Filter returns source with only the common, selected-agent, and explicit
// variables retained. It never logs or otherwise renders a value.
func Filter(source []string, agent string, extras []string) []string {
	return FilterForCommand(source, agent, "", extras)
}

// FilterForCommand is Filter with command-local cloud-mode assignments folded
// into the selected-agent policy. The command is parsed without evaluation;
// dynamic or unsupported syntax fails closed.
func FilterForCommand(source []string, agent, command string, extras []string) []string {
	allowed := allowedNames(source, agent, command, extras)
	return filterAllowed(source, allowed)
}

// FilterWithAuthSelectors applies a previously resolved set of conditional
// authentication selector names. It is used by durable teardown handles: the
// selector names can safely be stored, while the credential values remain only
// in the current process environment.
func FilterWithAuthSelectors(source []string, agent string, selectors, extras []string) ([]string, error) {
	normalized, err := NormalizeAuthSelectors(agent, selectors)
	if err != nil {
		return nil, err
	}
	allowed := allowedNamesWithAuthSelectors(agent, normalized, extras)
	return filterAllowed(source, allowed), nil
}

func filterAllowed(source []string, allowed map[string]struct{}) []string {
	out := make([]string, 0, len(source))
	for _, entry := range source {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !allowedName(allowed, name) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// ImportNames returns the deterministic variable-name list tmux should import
// from a filtered client when it creates a session on an existing server. The
// values remain solely in the client environment; this list is safe for argv.
// Names absent from source are included so tmux marks stale server values as
// removed instead of reviving an old credential in a new pane.
func ImportNames(source []string, agent string, extras []string) []string {
	return ImportNamesForCommand(source, agent, "", extras)
}

// ImportNamesForCommand is ImportNames with the same command-local cloud-mode
// policy as FilterForCommand.
func ImportNamesForCommand(source []string, agent, command string, extras []string) []string {
	allowed := allowedNames(source, agent, command, extras)
	for _, entry := range source {
		name, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(name, "LC_") && validName(name) {
			allowed[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(allowed))
	for name := range allowed {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DockerCLIEnvironmentForForwarding returns the environment for the trusted,
// short-lived Docker client after the caller has authorized forwardNames for
// the selected image. Only Docker client/transport state and those exact names
// survive; af daemon tokens, other agents' credentials, and unrelated ambient
// variables are excluded. The caller must reject repository-controlled run
// arguments before adding any forwardNames, because Docker can copy any client
// variable named by an `-e NAME` argument.
func DockerCLIEnvironmentForForwarding(source, forwardNames []string) []string {
	allowed := make(map[string]struct{}, len(dockerClientNames)+len(dockerControlNames)+len(forwardNames))
	for name := range dockerClientNames {
		allowed[name] = struct{}{}
	}
	for name := range dockerControlNames {
		allowed[name] = struct{}{}
	}
	for _, name := range forwardNames {
		if validName(name) {
			allowed[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(source))
	for _, entry := range source {
		name, _, ok := strings.Cut(entry, "=")
		_, explicitlyAllowed := allowed[name]
		if ok && explicitlyAllowed {
			out = append(out, entry)
		}
	}
	return out
}

// DockerClientConnectionNames returns every present client authority or
// connection name that repository-controlled docker.run_args must not be able
// to request from the Docker CLI environment: daemon/context configuration,
// SSH transport identity, proxies, and custom CA paths. Proxy values may also
// cross through the separate image trust gate; the others remain client-only.
func DockerClientConnectionNames(source []string) []string {
	wanted := make(map[string]struct{}, len(dockerControlNames)+len(dockerSSHTransportNames)+len(dockerProxyNames)+len(dockerClientCAPathNames))
	for name := range dockerControlNames {
		wanted[name] = struct{}{}
	}
	for name := range dockerSSHTransportNames {
		wanted[name] = struct{}{}
	}
	for name := range dockerProxyNames {
		wanted[name] = struct{}{}
	}
	for name := range dockerClientCAPathNames {
		wanted[name] = struct{}{}
	}
	return presentNonEmptyNames(source, wanted)
}

// DockerControlEnvironment is the value-minimal environment for Docker calls
// whose arguments are wholly constructed by af (exec/cp/port/rm). It includes
// client connection state but no agent, GitHub, or explicit session values.
func DockerControlEnvironment(source []string) []string {
	return DockerCLIEnvironmentForForwarding(source, DockerClientConnectionNames(source))
}

// DockerForwardNames returns the host variable names whose values should be
// copied into a container. By default, container-native basics such as HOME and
// PATH stay owned by the image; only authentication/network variables cross.
// Global explicit extensions are the deliberate escape hatch and may name any
// valid variable. The caller must authorize the selected image before using the
// returned names.
func DockerForwardNames(source []string, agent string, extras []string) []string {
	forward := make(map[string]struct{})
	for name := range dockerAgentNames[agent] {
		forward[name] = struct{}{}
	}
	for _, name := range extras {
		if validName(name) {
			forward[name] = struct{}{}
		}
	}
	for name := range dockerProxyNames {
		forward[name] = struct{}{}
	}
	return presentNonEmptyNames(source, forward)
}

// DockerContainerEnvironmentSpecs returns the safe arguments for `docker run
// -e`. Approved names remain value-free so Docker reads them from its filtered
// client environment. Every other standard proxy spelling gets an explicit
// empty assignment: Docker otherwise injects proxy values from the client's
// config.json into new containers without any -e argument, bypassing the image
// trust preflight above this layer.
func DockerContainerEnvironmentSpecs(forwardNames []string) []string {
	forwarded := make(map[string]struct{}, len(forwardNames))
	for _, name := range forwardNames {
		if validName(name) {
			forwarded[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(forwarded)+len(dockerProxyNames))
	for name := range forwarded {
		out = append(out, name)
	}
	for name := range dockerProxyNames {
		if _, ok := forwarded[name]; !ok {
			out = append(out, name+"=")
		}
	}
	sort.Strings(out)
	return out
}

func presentNonEmptyNames(source []string, wanted map[string]struct{}) []string {
	present := make(map[string]struct{}, len(wanted))
	for _, entry := range source {
		name, value, ok := strings.Cut(entry, "=")
		if ok && value != "" {
			if _, allowed := wanted[name]; allowed {
				present[name] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(present))
	for name := range present {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func allowedNames(source []string, agent, command string, extras []string) map[string]struct{} {
	return allowedNamesWithAuthSelectors(agent, ResolveAuthSelectors(source, agent, command), extras)
}

func allowedNamesWithAuthSelectors(agent string, selectors, extras []string) map[string]struct{} {
	allowed := make(map[string]struct{}, len(commonNames)+len(extras)+16)
	for name := range commonNames {
		allowed[name] = struct{}{}
	}
	for name := range selectedAgentNames(agent, selectors) {
		allowed[name] = struct{}{}
	}
	for _, name := range extras {
		if validName(name) {
			allowed[name] = struct{}{}
		}
	}
	return allowed
}

func selectedAgentNames(agent string, selectors []string) map[string]struct{} {
	selected := make(map[string]struct{}, len(agentNames[agent])+16)
	for name := range agentNames[agent] {
		selected[name] = struct{}{}
	}
	selectorSet := nameSet(selectors...)
	for _, group := range conditionalAgentNames[agent] {
		if _, enabled := selectorSet[group.selector]; !enabled {
			continue
		}
		for name := range group.names {
			selected[name] = struct{}{}
		}
	}
	return selected
}

// ResolveAuthSelectors returns the deterministic names of the selected agent's
// conditional authentication modes that are effectively enabled. It persists
// no values: callers may safely retain these names as durable policy state.
func ResolveAuthSelectors(source []string, agent, command string) []string {
	var selectors []string
	for _, group := range conditionalAgentNames[agent] {
		enabled := environmentFlagEnabled(source, group.selector)
		if found, commandEnabled := commandEnvironmentFlagState(command, agent, group.selector); found {
			enabled = commandEnabled
		}
		if enabled {
			selectors = append(selectors, group.selector)
		}
	}
	sort.Strings(selectors)
	return selectors
}

// NormalizeAuthSelectors validates a stored selector-name snapshot against the
// selected agent's known conditional modes. Errors identify only the position,
// never the untrusted stored text, so an accidental NAME=value record cannot
// render a credential in a log.
func NormalizeAuthSelectors(agent string, selectors []string) ([]string, error) {
	allowed := make(map[string]struct{}, len(conditionalAgentNames[agent]))
	for _, group := range conditionalAgentNames[agent] {
		allowed[group.selector] = struct{}{}
	}
	set := make(map[string]struct{}, len(selectors))
	for idx, raw := range selectors {
		selector := strings.TrimSpace(raw)
		if _, ok := allowed[selector]; !ok {
			return nil, fmt.Errorf("invalid authentication selector name at position %d", idx+1)
		}
		set[selector] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for selector := range set {
		out = append(out, selector)
	}
	sort.Strings(out)
	return out, nil
}

func environmentFlagEnabled(source []string, name string) bool {
	prefix := name + "="
	for _, entry := range source {
		if !strings.HasPrefix(entry, prefix) {
			continue
		}
		return flagValueEnabled(strings.TrimPrefix(entry, prefix))
	}
	return false
}

func flagValueEnabled(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value != "" && value != "0" && value != "false" && value != "no" && value != "off"
}

func allowedName(allowed map[string]struct{}, name string) bool {
	if _, ok := allowed[name]; ok {
		return true
	}
	return strings.HasPrefix(name, "LC_") && validName(name)
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for idx, r := range name {
		if idx == 0 {
			if r != '_' && !asciiLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !asciiLetter(r) && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func asciiLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func nameSet(names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}
