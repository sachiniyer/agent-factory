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

func TestDockerForwardNamesCarriesNamesOnlyWhenPresent(t *testing.T) {
	source := []string{
		"HOME=/host/home",
		"GH_TOKEN=present",
		"OPENAI_API_KEY=present",
		"UNRELATED_DATABASE_KEY=present",
		"CUSTOM_PROVIDER_TOKEN=present",
	}
	got := DockerForwardNames(source, "codex", []string{"CUSTOM_PROVIDER_TOKEN"})
	want := []string{"CUSTOM_PROVIDER_TOKEN", "GH_TOKEN", "OPENAI_API_KEY"}
	if !slices.Equal(got, want) {
		t.Fatalf("DockerForwardNames() = %v, want %v", got, want)
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
