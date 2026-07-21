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
// when Claude's documented mode selector is enabled in the source environment.
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
	allowed := allowedNames(source, agent, extras)
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
	allowed := allowedNames(source, agent, extras)
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

// DockerCLIEnvironment is the environment for the trusted, short-lived docker
// client. It includes Docker connection selection in addition to the session
// allowlist because those values decide which daemon the CLI contacts.
func DockerCLIEnvironment(source []string, agent string, extras []string) []string {
	allowed := allowedNames(source, agent, extras)
	for name := range dockerControlNames {
		allowed[name] = struct{}{}
	}
	out := make([]string, 0, len(source))
	for _, entry := range source {
		name, _, ok := strings.Cut(entry, "=")
		if ok && allowedName(allowed, name) {
			out = append(out, entry)
		}
	}
	return out
}

// DockerForwardNames returns the host variable names whose values should be
// copied into a container. Container-native basics such as HOME and PATH stay
// owned by the image; only authentication/network variables and explicit
// extensions cross the host/container boundary.
func DockerForwardNames(source []string, agent string, extras []string) []string {
	forward := make(map[string]struct{})
	for name := range selectedAgentNames(source, agent) {
		forward[name] = struct{}{}
	}
	for _, name := range extras {
		forward[name] = struct{}{}
	}
	for _, name := range []string{
		"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
	} {
		forward[name] = struct{}{}
	}
	present := make(map[string]struct{})
	for _, entry := range source {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, wanted := forward[name]; wanted {
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

func allowedNames(source []string, agent string, extras []string) map[string]struct{} {
	allowed := make(map[string]struct{}, len(commonNames)+len(extras)+16)
	for name := range commonNames {
		allowed[name] = struct{}{}
	}
	for name := range selectedAgentNames(source, agent) {
		allowed[name] = struct{}{}
	}
	for _, name := range extras {
		if validName(name) {
			allowed[name] = struct{}{}
		}
	}
	return allowed
}

func selectedAgentNames(source []string, agent string) map[string]struct{} {
	selected := make(map[string]struct{}, len(agentNames[agent])+16)
	for name := range agentNames[agent] {
		selected[name] = struct{}{}
	}
	for _, group := range conditionalAgentNames[agent] {
		if !environmentFlagEnabled(source, group.selector) {
			continue
		}
		for name := range group.names {
			selected[name] = struct{}{}
		}
	}
	return selected
}

func environmentFlagEnabled(source []string, name string) bool {
	prefix := name + "="
	for _, entry := range source {
		if !strings.HasPrefix(entry, prefix) {
			continue
		}
		value := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(entry, prefix)))
		return value != "" && value != "0" && value != "false" && value != "no" && value != "off"
	}
	return false
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
