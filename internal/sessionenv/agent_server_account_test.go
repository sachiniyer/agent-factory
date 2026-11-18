package sessionenv

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// execCaptured runs an agent-server handoff with processExec stubbed and returns
// the environment it would have exec'd with. The pane bookkeeping seams are
// stubbed too: execAgentServer calls stampPaneRootPid first, and when the test
// runs inside an af pane (TMUX_PANE set) the real write would run
// `tmux set-hook -p`/`set-option -p` on that LIVE pane — no sessionenv test may
// reach paneBookkeepingWrite or paneStampRead unstubbed (#7582 review).
func execCaptured(t *testing.T, run func() error) (env []string, err error) {
	t.Helper()
	stop := errors.New("stop before exec")
	previous := processExec
	prevWrite, prevRead := paneBookkeepingWrite, paneStampRead
	processExec = func(_ string, _ []string, environ []string) error {
		env = append([]string(nil), environ...)
		return stop
	}
	paneBookkeepingWrite = func(context.Context, string) error { return nil }
	paneStampRead = func(context.Context, string) (string, error) { return "", nil }
	t.Cleanup(func() {
		processExec = previous
		paneBookkeepingWrite, paneStampRead = prevWrite, prevRead
	})
	err = run()
	if errors.Is(err, stop) {
		err = nil
	}
	return env, err
}

// #5863: an account-bound agent-server keeps only its account's credentials. Every
// competing credential ApplyAccount subtracts for the agent, set ambient in the
// host/image environment, is gone (or defined empty) in the exec'd environment,
// and the delivered account root survives.
func TestAgentServerAccountExecDropsEveryCompetingCredential(t *testing.T) {
	serverArgs := func(agent string) []string {
		return []string{"agent-server", "--listen", ":1", "--repo", "/r", "--title", "t", "--program", agent, "--program-resolved"}
	}
	for _, agent := range AccountAgents() {
		t.Run(agent, func(t *testing.T) {
			configVar, _ := SupportsAccounts(agent)
			for name := range accountCredentialNames[agent] {
				t.Setenv(name, "ambient-"+name)
			}
			t.Setenv(configVar, "/scope/account")
			env, err := execCaptured(t, func() error {
				return agentServerAccountExecInvocation(append([]string{agent, "0"}, serverArgs(agent)...))
			})
			if err != nil {
				t.Fatal(err)
			}
			for name := range accountCredentialNames[agent] {
				if v, _ := envValue(env, name); v != "" {
					t.Errorf("ambient %s survived into the account-bound agent-server", name)
				}
			}
			if v, _ := envValue(env, configVar); v != "/scope/account" {
				t.Errorf("%s = %q, want the delivered account root", configVar, v)
			}
		})
	}
}

// Control: the plain handoff, used for a session WITHOUT an account, keeps the
// agent's credential as before.
func TestAgentServerPlainExecStillKeepsTheAgentCredential(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "fixture")
	env, err := execCaptured(t, func() error {
		return agentServerExecInvocation([]string{"0", "agent-server", "--listen", ":1", "--repo", "/r", "--title", "t", "--program", "codex", "--program-resolved"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "OPENAI_API_KEY=fixture") {
		t.Fatal("a session without an account lost its credential")
	}
}

// Fail closed: no delivered root, an unknown agent, or an active cloud mode is a
// refusal, never a launch on the ambient identity.
func TestAgentServerAccountExecRefusesWhenItCannotScope(t *testing.T) {
	args := []string{"0", "agent-server", "--listen", ":1", "--repo", "/r", "--title", "t", "--program", "claude", "--program-resolved"}
	t.Run("no root", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "ambient")
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		env, err := execCaptured(t, func() error { return agentServerAccountExecInvocation(append([]string{"claude"}, args...)) })
		if err == nil || env != nil {
			t.Fatalf("expected refusal before exec, err=%v env=%v", err, env != nil)
		}
	})
	t.Run("unknown agent", func(t *testing.T) {
		if _, err := execCaptured(t, func() error { return agentServerAccountExecInvocation(append([]string{"nope"}, args...)) }); err == nil {
			t.Fatal("expected refusal")
		}
	})
	t.Run("cloud mode", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", "/scope/account")
		t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
		if _, err := execCaptured(t, func() error { return agentServerAccountExecInvocation(append([]string{"claude"}, args...)) }); err == nil {
			t.Fatal("expected refusal under an active cloud mode")
		}
	})
}

// #7555: the keep-if-set names' whole hop chain, composed exactly as the docker
// and ssh backends run it. The wrap's extras are the operator's passthrough
// ONLY: an image ENV or run_args value rides no allowlist, so FilterForCommand
// drops it before ScopeAgentServerAccount can mistake it for a claim — and an
// absent name is installed defined-empty there. The inner pane's --session-env
// admission (the names AccountLoaderFilledNames lists) is what carries the
// decided value to the agent; naming them on that channel is the fix for R6.
func TestAgentServerLoaderNames_DroppedAtTheWrapPinnedAtTheAgent(t *testing.T) {
	loader := AccountLoaderFilledNames("gemini")
	// repoSet is the container-env shape image ENV / docker.run_args produce:
	// every name the operator did not forward carries a repository-chosen value.
	// Names in skip are left out — a forwarded name's -e overrides the image
	// value at docker run, so the container env holds the operator's value only.
	repoSet := func(skip ...string) []string {
		env := []string{"PATH=/bin", "GEMINI_CLI_HOME=/scope/account"}
		for _, name := range loader {
			if !slices.Contains(skip, name) {
				env = append(env, name+"=image-set")
			}
		}
		return env
	}
	assertPinned := func(t *testing.T, agentEnv []string) {
		t.Helper()
		for _, name := range loader {
			v, ok := envValue(agentEnv, name)
			require.True(t, ok, "%s: the decided value never reached the agent's environment", name)
			require.Empty(t, v, "%s: an image/run_args value must be dropped and then pinned EMPTY at the agent", name)
		}
	}

	t.Run("unforwarded: image values die at the wrap, pins reach the agent", func(t *testing.T) {
		wrapped := FilterForCommand(repoSet(), "gemini", "gemini", nil)
		for _, name := range loader {
			_, present := envValue(wrapped, name)
			require.False(t, present, "%s survived the wrap filter: presence there would read as the operator's claim", name)
		}
		scoped, err := ScopeAgentServerAccount(wrapped, "gemini", "gemini")
		require.NoError(t, err)
		agentEnv := FilterForCommand(scoped, "gemini", "gemini", loader)
		assertPinned(t, agentEnv)
	})

	t.Run("forwarded: the claim channel admits exactly the operator's value", func(t *testing.T) {
		passthrough := []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"}
		env := append(repoSet("GIT_CONFIG_COUNT"), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=/w")
		wrapped := FilterForCommand(env, "gemini", "gemini", passthrough)
		if v, _ := envValue(wrapped, "GIT_CONFIG_COUNT"); v != "1" {
			t.Fatalf("premise: the forwarded value must survive the wrap filter, got %q", v)
		}
		scoped, err := ScopeAgentServerAccount(wrapped, "gemini", "gemini")
		require.NoError(t, err)
		agentEnv := FilterForCommand(scoped, "gemini", "gemini",
			append(slices.Clone(loader), passthrough...))
		if v, _ := envValue(agentEnv, "GIT_CONFIG_COUNT"); v != "1" {
			t.Errorf("GIT_CONFIG_COUNT = %q at the agent: the forwarded value is the operator's own claim", v)
		}
		for _, name := range []string{"CLOUD_SHELL", "GCE_METADATA_IP", "GCE_METADATA_HOST", "GIT_CONFIG_PARAMETERS", "GEMINI_CLI_USE_COMPUTE_ADC"} {
			v, ok := envValue(agentEnv, name)
			require.True(t, ok && v == "", "%s: unforwarded names stay pinned empty at the agent", name)
		}
	})
}

// The same property at the exec-boundary level: the ambient environment stands
// in for image/run_args-set values. A name admitted through the wrap's extras
// is kept; one that is not is filtered out before the scope and comes back
// defined-empty.
func TestAgentServerAccountExec_LoaderNamesKeepIfSet(t *testing.T) {
	serverArgs := []string{"agent-server", "--listen", ":1", "--repo", "/r", "--title", "t", "--program", "gemini", "--program-resolved"}
	loader := AccountLoaderFilledNames("gemini")

	t.Run("forwarded", func(t *testing.T) {
		t.Setenv("GEMINI_CLI_HOME", "/scope/account")
		t.Setenv("GIT_CONFIG_COUNT", "1")
		t.Setenv("CLOUD_SHELL", "ambient-not-a-claim")
		env, err := execCaptured(t, func() error {
			return agentServerAccountExecInvocation(append([]string{"gemini", "1", "GIT_CONFIG_COUNT"}, serverArgs...))
		})
		require.NoError(t, err)
		if v, ok := envValue(env, "GIT_CONFIG_COUNT"); !ok || v != "1" {
			t.Errorf("GIT_CONFIG_COUNT = %q (present %v): the forwarded claim must be kept", v, ok)
		}
		if v, ok := envValue(env, "CLOUD_SHELL"); !ok || v != "" {
			t.Errorf("CLOUD_SHELL = %q (present %v): an ambient value that was never forwarded is not a claim — pinned empty", v, ok)
		}
	})

	t.Run("unforwarded", func(t *testing.T) {
		t.Setenv("GEMINI_CLI_HOME", "/scope/account")
		for _, name := range loader {
			t.Setenv(name, "ambient-not-a-claim")
		}
		env, err := execCaptured(t, func() error {
			return agentServerAccountExecInvocation(append([]string{"gemini", "0"}, serverArgs...))
		})
		require.NoError(t, err)
		for _, name := range loader {
			v, ok := envValue(env, name)
			require.True(t, ok, "%s must reach the agent-server defined", name)
			require.Empty(t, v, "%s: an ambient value that was never forwarded is not a claim — pinned empty", name)
		}
	})
}

func TestWrapAgentServerAccountCommandUsesTheAccountMarker(t *testing.T) {
	args := []string{"agent-server", "--listen", ":1", "--repo", "/r", "--title", "t", "--program", "claude", "--program-resolved"}
	got, err := WrapAgentServerAccountCommand("/af", "claude", nil, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, AgentServerAccountExecMarker) || strings.Contains(got, " "+AgentServerExecMarker+" ") {
		t.Fatalf("wrapped = %q", got)
	}
	if _, err := WrapAgentServerAccountCommand("/af", "nope", nil, args); err == nil {
		t.Fatal("an agent without account support must not wrap")
	}
}
