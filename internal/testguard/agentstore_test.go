package testguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agentStoreCase is one real root the tripwire must fence. env is the ambient
// environment the developer runs with, and file is where af's skill lands
// under it.
type agentStoreCase struct {
	name string
	env  func(t *testing.T, home string)
	file func(home string) string
}

func agentStoreCases() []agentStoreCase {
	unsetOverrides := func(t *testing.T, _ string) {
		unsetForTest(t, "CODEX_HOME")
		unsetForTest(t, "GEMINI_CLI_HOME")
	}
	skill := func(parts ...string) func(string) string {
		return func(home string) string {
			return filepath.Join(append(append([]string{home}, parts...), afSkillDirName, "SKILL.md")...)
		}
	}
	return []agentStoreCase{
		{name: "codex under HOME", env: unsetOverrides, file: skill(".codex", "skills")},
		{name: "codex under CODEX_HOME", env: func(t *testing.T, home string) {
			unsetOverrides(t, home)
			t.Setenv("CODEX_HOME", filepath.Join(home, "codex-store"))
		}, file: skill("codex-store", "skills")},
		{name: "gemini under HOME", env: unsetOverrides, file: skill(".gemini", "skills")},
		{name: "gemini under GEMINI_CLI_HOME", env: func(t *testing.T, home string) {
			unsetOverrides(t, home)
			t.Setenv("GEMINI_CLI_HOME", filepath.Join(home, "gemini-root"))
		}, file: skill("gemini-root", ".gemini", "skills")},
		{name: "amp", env: unsetOverrides, file: skill(".config", "amp", "skills")},
		{name: "devin", env: unsetOverrides, file: skill(".config", "devin", "skills")},
	}
}

func writeFileAll(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestConfigTripwire_FencesRealAgentStores covers each real root af writes
// into, for each of the three changes a before/after snapshot can see. The
// DELETED row is the one that happens in practice. A sandboxed package runs
// with global_agent_skills off, so a test that reaches the developer's root runs
// the declined-setting cleanup and removes the skill the developer turned on.
func TestConfigTripwire_FencesRealAgentStores(t *testing.T) {
	mutations := []struct {
		name   string
		seed   bool
		mutate func(t *testing.T, path string)
		want   string
	}{
		{name: "deleted", seed: true, mutate: func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove %s: %v", path, err)
			}
		}, want: "DELETED"},
		{name: "modified", seed: true, mutate: func(t *testing.T, path string) {
			writeFileAll(t, path, "rewritten by a test")
		}, want: "MODIFIED"},
		{name: "created", mutate: func(t *testing.T, path string) {
			writeFileAll(t, path, "written by a test")
		}, want: "CREATED"},
	}
	for _, store := range agentStoreCases() {
		for _, mutation := range mutations {
			t.Run(store.name+"/"+mutation.name, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("AGENT_FACTORY_HOME", filepath.Join(home, "af"))
				store.env(t, home)
				path := store.file(home)
				if mutation.seed {
					writeFileAll(t, path, "the developer's af skill")
				}

				verify := ConfigTripwire()
				mutation.mutate(t, path)

				err := verify()
				if err == nil {
					t.Fatalf("tripwire did not fire after %s was %s", path, strings.ToLower(mutation.want))
				}
				if !strings.Contains(err.Error(), mutation.want) || !strings.Contains(err.Error(), path) {
					t.Fatalf("tripwire error %q should name %s as %s", err, path, mutation.want)
				}
			})
		}
	}
}

// TestConfigTripwire_IgnoresATestsOwnAgentRoot is the contract for legitimate
// tests: one that points CODEX_HOME, GEMINI_CLI_HOME or HOME at its own temp
// dir and writes skills there has not reached a real root. It must not trip the
// check, even though the environment changed after the tripwire was armed.
func TestConfigTripwire_IgnoresATestsOwnAgentRoot(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(realHome, "af"))
	unsetForTest(t, "CODEX_HOME")
	unsetForTest(t, "GEMINI_CLI_HOME")
	for _, store := range agentStoreCases() {
		writeFileAll(t, store.file(realHome), "the developer's af skill")
	}

	verify := ConfigTripwire()

	ownHome := t.TempDir()
	t.Setenv("HOME", ownHome)
	t.Setenv("CODEX_HOME", filepath.Join(ownHome, "codex"))
	t.Setenv("GEMINI_CLI_HOME", filepath.Join(ownHome, "gemini"))
	for _, own := range []string{
		filepath.Join(ownHome, "codex", "skills", afSkillDirName, "SKILL.md"),
		filepath.Join(ownHome, "gemini", ".gemini", "skills", afSkillDirName, "SKILL.md"),
		filepath.Join(ownHome, ".config", "amp", "skills", afSkillDirName, "SKILL.md"),
		filepath.Join(ownHome, ".config", "devin", "skills", afSkillDirName, "SKILL.md"),
	} {
		writeFileAll(t, own, "written by the test into its own root")
	}

	if err := verify(); err != nil {
		t.Fatalf("a test writing into its own agent roots tripped the check: %v", err)
	}
}

func TestConfigTripwire_AgentStoreDisabledByEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(home, "af"))
	unsetForTest(t, "CODEX_HOME")
	unsetForTest(t, "GEMINI_CLI_HOME")
	t.Setenv("AF_DISABLE_AGENT_STORE_TRIPWIRE", "1")
	path := filepath.Join(home, ".codex", "skills", afSkillDirName, "SKILL.md")
	writeFileAll(t, path, "the developer's af skill")

	verify := ConfigTripwire()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
	if err := verify(); err != nil {
		t.Fatalf("AF_DISABLE_AGENT_STORE_TRIPWIRE=1 should disable the agent-store check; got %v", err)
	}
}

// TestConfigTripwire_ReportsConfigAndAgentStoreTogether pins that folding the
// agent-store check into ConfigTripwire kept the config half working. When both
// fire, both are reported, so fixing one does not hide the other for a run.
func TestConfigTripwire_ReportsConfigAndAgentStoreTogether(t *testing.T) {
	configPath := sandbox(t)
	home := os.Getenv("HOME")
	skillPath := filepath.Join(home, ".config", "devin", "skills", afSkillDirName, "SKILL.md")
	writeFileAll(t, skillPath, "the developer's af skill")

	verify := ConfigTripwire()
	writeFileAll(t, configPath, `{}`)
	if err := os.Remove(skillPath); err != nil {
		t.Fatalf("remove %s: %v", skillPath, err)
	}

	err := verify()
	if err == nil {
		t.Fatal("tripwire did not fire after both the config and a skill changed")
	}
	for _, want := range []string{"config tripwire", configPath, "agent store tripwire", skillPath} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("tripwire error %q should mention %q", err, want)
		}
	}
}
