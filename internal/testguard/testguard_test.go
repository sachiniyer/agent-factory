package testguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandbox points the tripwire's ambient resolution at a temp dir and returns
// the config.json path inside it.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	return filepath.Join(dir, "config.json")
}

func TestConfigTripwire_FiresOnModification(t *testing.T) {
	path := sandbox(t)
	if err := os.WriteFile(path, []byte(`{"detach_keys":"ctrl-]"}`), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	verify := ConfigTripwire()
	if err := os.WriteFile(path, []byte(`{"detach_keys":"ctrl-w"}`), 0644); err != nil {
		t.Fatalf("mutate config: %v", err)
	}

	err := verify()
	if err == nil {
		t.Fatalf("tripwire did not fire after the config was modified")
	}
	if !strings.Contains(err.Error(), "MODIFIED") {
		t.Fatalf("tripwire error %q should name the modification", err)
	}
}

func TestConfigTripwire_FiresOnDeletion(t *testing.T) {
	path := sandbox(t)
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	verify := ConfigTripwire()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove config: %v", err)
	}

	err := verify()
	if err == nil {
		t.Fatalf("tripwire did not fire after the config was deleted")
	}
	if !strings.Contains(err.Error(), "DELETED") {
		t.Fatalf("tripwire error %q should name the deletion", err)
	}
}

func TestConfigTripwire_FiresOnCreationFromAbsent(t *testing.T) {
	path := sandbox(t)

	verify := ConfigTripwire()
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatalf("create config: %v", err)
	}

	err := verify()
	if err == nil {
		t.Fatalf("tripwire did not fire after a config was materialized into an empty real home")
	}
	if !strings.Contains(err.Error(), "CREATED") {
		t.Fatalf("tripwire error %q should name the creation", err)
	}
}

func TestConfigTripwire_SilentWhenUntouched(t *testing.T) {
	path := sandbox(t)
	if err := os.WriteFile(path, []byte(`{"auto_yes":true}`), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	verify := ConfigTripwire()
	if err := verify(); err != nil {
		t.Fatalf("tripwire fired on an untouched config: %v", err)
	}
}

func TestConfigTripwire_SilentWhenAbsentStaysAbsent(t *testing.T) {
	sandbox(t)
	verify := ConfigTripwire()
	if err := verify(); err != nil {
		t.Fatalf("tripwire fired on a home with no config at all: %v", err)
	}
}

func TestConfigTripwire_DisabledByEnv(t *testing.T) {
	path := sandbox(t)
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	t.Setenv("AF_DISABLE_CONFIG_TRIPWIRE", "1")

	verify := ConfigTripwire()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	if err := verify(); err != nil {
		t.Fatalf("disabled tripwire must not fire, got: %v", err)
	}
}

func TestConfigTripwire_ExpandsTildeHome(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("AGENT_FACTORY_HOME", "~/af-home")
	require := filepath.Join(fakeHome, "af-home", "config.json")

	if got := ambientConfigPath(); got != require {
		t.Fatalf("ambientConfigPath() = %q, want %q", got, require)
	}
}

func TestConfigTripwire_FallsBackToDotAgentFactory(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("AGENT_FACTORY_HOME", "")

	want := filepath.Join(fakeHome, ".agent-factory", "config.json")
	if got := ambientConfigPath(); got != want {
		t.Fatalf("ambientConfigPath() = %q, want %q", got, want)
	}
}
