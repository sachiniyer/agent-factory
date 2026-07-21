package sessionenv

import (
	"slices"
	"strings"
	"testing"
)

func TestAgentForCommandRequiresLiteralAgentInvocation(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "bare agent", command: "codex --model o3", want: "codex"},
		{name: "agent path", command: "/opt/bin/claude --permission-mode plan", want: "claude"},
		{name: "literal assignment", command: "CODEX_HOME=/tmp/codex codex", want: "codex"},
		{name: "exec", command: "exec -- gemini --model flash", want: "gemini"},
		{name: "env", command: "env -i HOME=/tmp aider --model sonnet", want: "aider"},
		{
			name:    "generated agent server",
			command: "/srv/af agent-server --listen :43110 --repo /workspace --title test --program 'opencode --model test' --program-resolved --session-env CUSTOM_TOKEN",
			want:    "opencode",
		},
		{name: "agent name used as data", command: "./collect codex"},
		{name: "agent server title lookalike", command: "/srv/af agent-server --listen :43110 --repo /workspace --title codex"},
		{name: "compound command", command: "collect; codex"},
		{name: "redirect", command: "codex >output"},
		{name: "dynamic argument", command: "codex --model $MODEL"},
		{name: "unsupported wrapper", command: "command codex"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := AgentForCommand(test.command); got != test.want {
				t.Fatalf("AgentForCommand(%q) = %q, want %q", test.command, got, test.want)
			}
		})
	}
}

func TestFilterScopesAuthenticationToSelectedAgent(t *testing.T) {
	source := []string{
		"PATH=/usr/bin",
		"LC_ALL=C",
		"GH_TOKEN=present",
		"OPENAI_API_KEY=present",
		"ANTHROPIC_API_KEY=present",
		"UNRELATED_DATABASE_KEY=present",
	}
	got := Filter(source, "codex", nil)

	for _, want := range []string{"PATH=/usr/bin", "LC_ALL=C", "GH_TOKEN=present", "OPENAI_API_KEY=present"} {
		if !slices.Contains(got, want) {
			t.Fatalf("filtered environment is missing %s", want[:len(want)-len("present")])
		}
	}
	for _, denied := range []string{"ANTHROPIC_API_KEY=present", "UNRELATED_DATABASE_KEY=present"} {
		if slices.Contains(got, denied) {
			t.Fatalf("filtered environment retained disallowed variable %s", denied[:len(denied)-len("=present")])
		}
	}
}

func TestFilterAllowsExplicitExactName(t *testing.T) {
	got := Filter([]string{"CUSTOM_PROVIDER_TOKEN=present", "OTHER_TOKEN=present"}, "codex", []string{"CUSTOM_PROVIDER_TOKEN"})
	if !slices.Contains(got, "CUSTOM_PROVIDER_TOKEN=present") {
		t.Fatal("explicit pass-through variable was removed")
	}
	if slices.Contains(got, "OTHER_TOKEN=present") {
		t.Fatal("unlisted variable survived the default-deny filter")
	}
}

func TestImportNamesIncludesAbsentAllowedNamesAndDynamicLocale(t *testing.T) {
	got := ImportNames([]string{"LC_MESSAGES=C", "UNRELATED_DATABASE_KEY=present"}, "codex", []string{"CUSTOM_PROVIDER_TOKEN"})
	for _, want := range []string{"PATH", "OPENAI_API_KEY", "CUSTOM_PROVIDER_TOKEN", "LC_MESSAGES"} {
		if !slices.Contains(got, want) {
			t.Fatalf("tmux import names omitted %s", want)
		}
	}
	for _, denied := range []string{"ANTHROPIC_API_KEY", "UNRELATED_DATABASE_KEY"} {
		if slices.Contains(got, denied) {
			t.Fatalf("tmux import names admitted %s", denied)
		}
	}
}

func TestFilterRequiresClaudeProviderSelectorForCloudCredentials(t *testing.T) {
	source := []string{
		"ANTHROPIC_API_KEY=present",
		"AWS_ACCESS_KEY_ID=present",
		"AWS_SECRET_ACCESS_KEY=present",
		"AZURE_CLIENT_SECRET=present",
	}
	got := Filter(source, "claude", nil)
	if !slices.Contains(got, "ANTHROPIC_API_KEY=present") {
		t.Fatal("Claude's direct authentication variable was removed")
	}
	for _, denied := range []string{"AWS_ACCESS_KEY_ID=present", "AWS_SECRET_ACCESS_KEY=present", "AZURE_CLIENT_SECRET=present"} {
		if slices.Contains(got, denied) {
			t.Fatalf("Claude inherited inactive provider credential %s", strings.SplitN(denied, "=", 2)[0])
		}
	}

	bedrockSource := append(source, "CLAUDE_CODE_USE_BEDROCK=1")
	bedrock := Filter(bedrockSource, "claude", nil)
	for _, want := range []string{"CLAUDE_CODE_USE_BEDROCK=1", "AWS_ACCESS_KEY_ID=present", "AWS_SECRET_ACCESS_KEY=present"} {
		if !slices.Contains(bedrock, want) {
			t.Fatalf("Claude Bedrock environment omitted %s", strings.SplitN(want, "=", 2)[0])
		}
	}
	if slices.Contains(bedrock, "AZURE_CLIENT_SECRET=present") {
		t.Fatal("Claude Bedrock mode inherited inactive Foundry credentials")
	}
}

func TestFilterForCommandHonorsLiteralClaudeCloudModeSelectors(t *testing.T) {
	source := []string{
		"AWS_ACCESS_KEY_ID=fixture",
		"AWS_SECRET_ACCESS_KEY=fixture",
		"AZURE_CLIENT_SECRET=fixture",
	}
	commands := []string{
		"CLAUDE_CODE_USE_BEDROCK=1 claude",
		"env CLAUDE_CODE_USE_BEDROCK=true claude",
		"env PATH=/opt/claude CLAUDE_CODE_USE_BEDROCK=1 claude",
		"env -i CLAUDE_CODE_USE_BEDROCK=1 claude",
		"/srv/af agent-server --listen :43110 --repo /workspace --title test --program 'CLAUDE_CODE_USE_BEDROCK=1 claude' --program-resolved",
		"exec /srv/af agent-server --listen 127.0.0.1:0 --repo /workspace --title test --program 'CLAUDE_CODE_USE_BEDROCK=1 claude' --program-resolved --session-env CUSTOM_TOKEN",
	}
	for _, command := range commands {
		got := FilterForCommand(source, "claude", command, nil)
		for _, want := range []string{"AWS_ACCESS_KEY_ID=fixture", "AWS_SECRET_ACCESS_KEY=fixture"} {
			if !slices.Contains(got, want) {
				t.Fatalf("literal cloud-mode command omitted %s", strings.SplitN(want, "=", 2)[0])
			}
		}
		if slices.Contains(got, "AZURE_CLIENT_SECRET=fixture") {
			t.Fatal("Bedrock command admitted an inactive Foundry credential")
		}
	}
}

func TestFilterForCommandFailsClosedOnDisabledOrDynamicCloudMode(t *testing.T) {
	source := []string{"AWS_ACCESS_KEY_ID=fixture"}
	commands := []string{
		"CLAUDE_CODE_USE_BEDROCK=0 claude",
		"CLAUDE_CODE_USE_BEDROCK=$MODE claude",
		"env CLAUDE_CODE_USE_BEDROCK=$MODE claude",
		"env CLAUDE_CODE_USE_BEDROCK=1 claude --model \"$MODEL\"",
		"CLAUDE_CODE_USE_BEDROCK=1 CLAUDE_CODE_USE_BEDROCK=off claude",
		"CLAUDE_CODE_USE_BEDROCK=1 env -u CLAUDE_CODE_USE_BEDROCK claude",
		"CLAUDE_CODE_USE_BEDROCK=1 env -u CLAUDE_CODE_USE_BEDROCK claude --model \"$MODEL\"",
		"helper --program 'CLAUDE_CODE_USE_BEDROCK=1 claude'",
		"claude || CLAUDE_CODE_USE_BEDROCK=1 claude",
		"CLAUDE_CODE_USE_BEDROCK=1 claude || claude",
		"echo env CLAUDE_CODE_USE_BEDROCK=1 claude; ./steal",
	}
	for _, command := range commands {
		if got := FilterForCommand(source, "claude", command, nil); len(got) != 0 {
			t.Fatalf("disabled or dynamic cloud-mode command admitted provider credentials: %q", command)
		}
	}

	exported := []string{"CLAUDE_CODE_USE_BEDROCK=1", "AWS_ACCESS_KEY_ID=fixture"}
	explicitlyDisabled := append([]string(nil), commands[:7]...)
	explicitlyDisabled = append(explicitlyDisabled, "env -i claude", "env --ignore-environment claude")
	for _, command := range explicitlyDisabled {
		if got := FilterForCommand(exported, "claude", command, nil); slices.Contains(got, "AWS_ACCESS_KEY_ID=fixture") {
			t.Fatalf("disabled or dynamic command override inherited an exported provider credential: %q", command)
		}
	}
}

func TestResolvedAuthSelectorsReapplyOnlyValidatedProviderPolicy(t *testing.T) {
	selectors := ResolveAuthSelectors(nil, "claude", "CLAUDE_CODE_USE_BEDROCK=1 claude")
	if !slices.Equal(selectors, []string{"CLAUDE_CODE_USE_BEDROCK"}) {
		t.Fatalf("resolved selectors = %v, want the Bedrock selector name", selectors)
	}

	got, err := FilterWithAuthSelectors([]string{
		"AWS_ACCESS_KEY_ID=fixture",
		"AWS_SECRET_ACCESS_KEY=fixture",
		"AZURE_CLIENT_SECRET=fixture",
	}, "claude", selectors, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AWS_ACCESS_KEY_ID=fixture", "AWS_SECRET_ACCESS_KEY=fixture"} {
		if !slices.Contains(got, want) {
			t.Fatalf("resolved Bedrock policy omitted %s", strings.SplitN(want, "=", 2)[0])
		}
	}
	if slices.Contains(got, "AZURE_CLIENT_SECRET=fixture") {
		t.Fatal("resolved Bedrock policy admitted Foundry credentials")
	}
}

func TestNormalizeAuthSelectorsRejectsUnknownNameWithoutRenderingIt(t *testing.T) {
	_, err := NormalizeAuthSelectors("claude", []string{"TOKEN=do-not-render"})
	if err == nil {
		t.Fatal("NormalizeAuthSelectors accepted an unknown selector")
	}
	if strings.Contains(err.Error(), "do-not-render") {
		t.Fatal("selector validation error rendered untrusted stored text")
	}
}

func TestNormalizeExtraNamesRejectsPatternsAndAssignments(t *testing.T) {
	for _, name := range []string{"PROVIDER_*", "TOKEN=value", "9TOKEN", "Å_TOKEN", ""} {
		if _, err := NormalizeExtraNames([]string{name}); err == nil {
			t.Fatalf("NormalizeExtraNames accepted invalid name %q", name)
		}
	}
}

func TestNormalizeExtraNamesDoesNotRenderInvalidAssignmentValue(t *testing.T) {
	_, err := NormalizeExtraNames([]string{"CUSTOM_TOKEN=do-not-render"})
	if err == nil {
		t.Fatal("NormalizeExtraNames accepted an assignment")
	}
	if strings.Contains(err.Error(), "do-not-render") {
		t.Fatal("validation error rendered an invalid assignment value")
	}
}

func TestDockerForwardNamesCarriesPortableAndExplicitNamesWhenPresent(t *testing.T) {
	source := []string{
		"HOME=/host/home",
		"CODEX_ACCESS_TOKEN=",
		"GH_TOKEN=present",
		"OPENAI_API_KEY=present",
		"HTTPS_PROXY=present",
		"UNRELATED_DATABASE_KEY=present",
		"CUSTOM_PROVIDER_TOKEN=present",
		"LC_PACKAGE_TOKEN=present",
	}
	extras := []string{"CUSTOM_PROVIDER_TOKEN", "GH_TOKEN", "HTTPS_PROXY", "LC_PACKAGE_TOKEN", "OPENAI_API_KEY"}
	got := DockerForwardNames(source, "codex", extras)
	want := []string{"CUSTOM_PROVIDER_TOKEN", "GH_TOKEN", "HTTPS_PROXY", "LC_PACKAGE_TOKEN", "OPENAI_API_KEY"}
	if !slices.Equal(got, want) {
		t.Fatalf("DockerForwardNames() = %v, want %v", got, want)
	}
	client := DockerCLIEnvironmentForForwarding(source, got)
	for _, wantEntry := range []string{"CUSTOM_PROVIDER_TOKEN=present", "GH_TOKEN=present", "HTTPS_PROXY=present", "LC_PACKAGE_TOKEN=present", "OPENAI_API_KEY=present"} {
		if !slices.Contains(client, wantEntry) {
			t.Fatalf("explicit Docker trust grant omitted %s", strings.SplitN(wantEntry, "=", 2)[0])
		}
	}
}

func TestDockerClientBaselineDoesNotReceiveBuiltInCredentials(t *testing.T) {
	source := []string{
		"PATH=/usr/bin",
		"GH_TOKEN=fixture",
		"OPENAI_API_KEY=fixture",
		"HTTPS_PROXY=fixture",
		"SSL_CERT_FILE=/fixture/ca.pem",
		"LC_SECRET_TOKEN=fixture",
	}
	for _, entry := range DockerCLIEnvironmentForForwarding(source, nil) {
		name, _, _ := strings.Cut(entry, "=")
		for _, denied := range []string{"GH_TOKEN", "OPENAI_API_KEY", "HTTPS_PROXY", "SSL_CERT_FILE", "LC_SECRET_TOKEN"} {
			if name == denied {
				t.Fatalf("Docker CLI environment exposed built-in variable %s to repo-controlled run arguments", name)
			}
		}
	}
	if got := DockerForwardNames(source, "codex", nil); !slices.Equal(got, []string{"HTTPS_PROXY", "OPENAI_API_KEY"}) {
		t.Fatalf("Docker trust preflight candidates = %v, want selected portable credentials and network values", got)
	}
	if got := DockerClientConnectionNames(source); !slices.Equal(got, []string{"HTTPS_PROXY", "SSL_CERT_FILE"}) {
		t.Fatalf("Docker client connection names = %v, want proxy and custom CA", got)
	}
	control := DockerControlEnvironment(source)
	for _, want := range []string{"PATH=/usr/bin", "HTTPS_PROXY=fixture", "SSL_CERT_FILE=/fixture/ca.pem"} {
		if !slices.Contains(control, want) {
			t.Fatalf("controlled Docker call omitted client connection entry %s", strings.SplitN(want, "=", 2)[0])
		}
	}
	for _, denied := range []string{"GH_TOKEN=fixture", "OPENAI_API_KEY=fixture"} {
		if slices.Contains(control, denied) {
			t.Fatalf("controlled Docker call retained session credential %s", strings.SplitN(denied, "=", 2)[0])
		}
	}
}

func TestDockerForwardNamesDoNotTreatHostPathsAsPortableCredentials(t *testing.T) {
	source := []string{
		"CODEX_ACCESS_TOKEN=present",
		"CODEX_HOME=/host/codex",
		"CODEX_CA_CERTIFICATE=/host/certs/codex.pem",
		"GH_TOKEN=present",
		"SSL_CERT_FILE=/host/certs/ca.pem",
		"HTTPS_PROXY=http://proxy.invalid",
	}
	got := DockerForwardNames(source, "codex", nil)
	for _, want := range []string{"CODEX_ACCESS_TOKEN", "HTTPS_PROXY"} {
		if !slices.Contains(got, want) {
			t.Fatalf("DockerForwardNames omitted portable value %s", want)
		}
	}
	for _, denied := range []string{"CODEX_HOME", "CODEX_CA_CERTIFICATE", "GH_TOKEN", "SSL_CERT_FILE"} {
		if slices.Contains(got, denied) {
			t.Fatalf("DockerForwardNames forwarded host-only path %s", denied)
		}
	}
}

func TestDockerContainerEnvironmentSpecsClearOnlyUnapprovedProxies(t *testing.T) {
	got := DockerContainerEnvironmentSpecs([]string{"CUSTOM_TOKEN", "HTTPS_PROXY"})
	for _, want := range []string{"CUSTOM_TOKEN", "HTTPS_PROXY", "HTTP_PROXY=", "FTP_PROXY=", "ftp_proxy="} {
		if !slices.Contains(got, want) {
			t.Fatalf("Docker container environment specs omitted %q: %v", want, got)
		}
	}
	if slices.Contains(got, "HTTPS_PROXY=") {
		t.Fatalf("Docker container environment specs cleared approved HTTPS_PROXY: %v", got)
	}
}

func TestDockerCLIEnvironmentContainsOnlyClientStateAndExplicitForwards(t *testing.T) {
	source := []string{
		"PATH=/usr/bin",
		"HOME=/host/home",
		"LANG=C.UTF-8",
		"DOCKER_HOST=unix:///run/docker.sock",
		"CODEX_ACCESS_TOKEN=present",
		"AF_DAEMON_TOKEN=control-plane-secret",
		"SSH_AUTH_SOCK=/run/user/1000/agent",
		"UNRELATED_DATABASE_KEY=present",
	}
	got := DockerCLIEnvironmentForForwarding(source, []string{"CODEX_ACCESS_TOKEN"})
	for _, want := range []string{"PATH=/usr/bin", "HOME=/host/home", "LANG=C.UTF-8", "DOCKER_HOST=unix:///run/docker.sock", "SSH_AUTH_SOCK=/run/user/1000/agent", "CODEX_ACCESS_TOKEN=present"} {
		if !slices.Contains(got, want) {
			t.Fatalf("Docker CLI environment omitted required entry %s", strings.SplitN(want, "=", 2)[0])
		}
	}
	for _, denied := range []string{"AF_DAEMON_TOKEN=control-plane-secret", "UNRELATED_DATABASE_KEY=present"} {
		if slices.Contains(got, denied) {
			t.Fatalf("Docker CLI environment retained unrelated entry %s", strings.SplitN(denied, "=", 2)[0])
		}
	}
}

func TestWrapCommandNeverRendersEnvironmentValues(t *testing.T) {
	wrapped, err := WrapCommand("/opt/af binary", "codex", []string{"CUSTOM_PROVIDER_TOKEN"}, "codex --model 'a b'")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"__af-session-env-exec", "CUSTOM_PROVIDER_TOKEN", "codex --model"} {
		if !strings.Contains(wrapped, want) {
			t.Fatalf("wrapped command missing %q", want)
		}
	}
}
