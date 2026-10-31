package sessionenv

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// execCaptured runs an agent-server handoff with processExec stubbed and returns
// the environment it would have exec'd with.
func execCaptured(t *testing.T, run func() error) (env []string, err error) {
	t.Helper()
	stop := errors.New("stop before exec")
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		env = append([]string(nil), environ...)
		return stop
	}
	t.Cleanup(func() { processExec = previous })
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
