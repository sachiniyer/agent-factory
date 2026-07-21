package sessionenv

import (
	"slices"
	"strings"
	"testing"
)

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
		"env CLAUDE_CODE_USE_BEDROCK=1 claude --model \"$MODEL\"",
		"/srv/af agent-server --program 'CLAUDE_CODE_USE_BEDROCK=1 claude'",
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
		"CLAUDE_CODE_USE_BEDROCK=1 CLAUDE_CODE_USE_BEDROCK=off claude",
		"CLAUDE_CODE_USE_BEDROCK=1 env -u CLAUDE_CODE_USE_BEDROCK claude",
		"CLAUDE_CODE_USE_BEDROCK=1 env -u CLAUDE_CODE_USE_BEDROCK claude --model \"$MODEL\"",
		"helper --program 'CLAUDE_CODE_USE_BEDROCK=1 claude'",
	}
	for _, command := range commands {
		if got := FilterForCommand(source, "claude", command, nil); len(got) != 0 {
			t.Fatalf("disabled or dynamic cloud-mode command admitted provider credentials: %q", command)
		}
	}

	exported := []string{"CLAUDE_CODE_USE_BEDROCK=1", "AWS_ACCESS_KEY_ID=fixture"}
	for _, command := range commands[:6] {
		if got := FilterForCommand(exported, "claude", command, nil); slices.Contains(got, "AWS_ACCESS_KEY_ID=fixture") {
			t.Fatalf("disabled or dynamic command override inherited an exported provider credential: %q", command)
		}
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

func TestDockerForwardNamesCarriesOnlyExplicitNamesWhenPresent(t *testing.T) {
	source := []string{
		"HOME=/host/home",
		"GH_TOKEN=present",
		"OPENAI_API_KEY=present",
		"HTTPS_PROXY=present",
		"UNRELATED_DATABASE_KEY=present",
		"CUSTOM_PROVIDER_TOKEN=present",
	}
	extras := []string{"CUSTOM_PROVIDER_TOKEN", "GH_TOKEN", "HTTPS_PROXY", "OPENAI_API_KEY"}
	got := DockerForwardNames(source, "codex", extras)
	want := []string{"CUSTOM_PROVIDER_TOKEN", "GH_TOKEN", "HTTPS_PROXY", "OPENAI_API_KEY"}
	if !slices.Equal(got, want) {
		t.Fatalf("DockerForwardNames() = %v, want %v", got, want)
	}
	client := DockerCLIEnvironment(source, "codex", extras)
	for _, wantEntry := range []string{"CUSTOM_PROVIDER_TOKEN=present", "GH_TOKEN=present", "HTTPS_PROXY=present", "OPENAI_API_KEY=present"} {
		if !slices.Contains(client, wantEntry) {
			t.Fatalf("explicit Docker trust grant omitted %s", strings.SplitN(wantEntry, "=", 2)[0])
		}
	}
}

func TestDockerRepoSelectedImageDoesNotReceiveBuiltInCredentials(t *testing.T) {
	source := []string{
		"PATH=/usr/bin",
		"GH_TOKEN=fixture",
		"OPENAI_API_KEY=fixture",
		"HTTPS_PROXY=fixture",
		"SSL_CERT_FILE=/fixture/ca.pem",
	}
	for _, entry := range DockerCLIEnvironment(source, "codex", nil) {
		name, _, _ := strings.Cut(entry, "=")
		for _, denied := range []string{"GH_TOKEN", "OPENAI_API_KEY", "HTTPS_PROXY", "SSL_CERT_FILE"} {
			if name == denied {
				t.Fatalf("Docker CLI environment exposed built-in variable %s to repo-controlled run arguments", name)
			}
		}
	}
	if got := DockerForwardNames(source, "codex", nil); len(got) != 0 {
		t.Fatalf("repo-selected Docker image received built-in credential names: %v", got)
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
