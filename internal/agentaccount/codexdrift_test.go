package agentaccount

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCodexConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRuntimeDriftKey(t *testing.T) {
	base := t.TempDir()
	src, tgt := filepath.Join(base, "src"), filepath.Join(base, "tgt")

	t.Run("equal runtime settings are comparable", func(t *testing.T) {
		writeCodexConfig(t, src, "model = \"gpt-5\"\napproval_policy = \"on-request\"\nsandbox_mode = \"read-only\"\n")
		writeCodexConfig(t, tgt, "model = \"gpt-5\"\napproval_policy = \"on-request\"\nsandbox_mode = \"read-only\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt); key != "" || inc {
			t.Fatalf("got (%q,%v), want (\"\",false)", key, inc)
		}
	})
	t.Run("different model names the drift", func(t *testing.T) {
		writeCodexConfig(t, tgt, "model = \"gpt-5-mini\"\napproval_policy = \"on-request\"\nsandbox_mode = \"read-only\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt); key != "model" || inc {
			t.Fatalf("got (%q,%v), want (\"model\",false)", key, inc)
		}
	})
	t.Run("source key the target omits is drift", func(t *testing.T) {
		// No presence gap is presumed seedable: prep fills from ambient
		// prompts, not from source settings (#6817 review).
		writeCodexConfig(t, tgt, "approval_policy = \"on-request\"\nsandbox_mode = \"read-only\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt); key != "model" || inc {
			t.Fatalf("got (%q,%v), want (\"model\",false)", key, inc)
		}
	})
	t.Run("unparseable target is incomparable", func(t *testing.T) {
		writeCodexConfig(t, tgt, "not = [toml\n")
		if _, inc := CodexRuntimeDriftKey(src, tgt); !inc {
			t.Fatalf("unparseable target was comparable")
		}
	})
	t.Run("missing target config is drift when source sets keys", func(t *testing.T) {
		// An absent file is provably empty, not unverifiable — and a source
		// that sets runtime keys cannot be presumed equal to an empty target.
		if err := os.Remove(filepath.Join(tgt, "config.toml")); err != nil {
			t.Fatal(err)
		}
		if key, inc := CodexRuntimeDriftKey(src, tgt); key != "model" || inc {
			t.Fatalf("got (%q,%v), want (\"model\",false)", key, inc)
		}
	})
	t.Run("empty source and empty target compare equal", func(t *testing.T) {
		writeCodexConfig(t, src, "")
		if key, inc := CodexRuntimeDriftKey(src, tgt); key != "" || inc {
			t.Fatalf("got (%q,%v), want (\"\",false)", key, inc)
		}
	})
	t.Run("source effort the target lacks is drift", func(t *testing.T) {
		writeCodexConfig(t, src, "model = \"gpt-5\"\nmodel_reasoning_effort = \"high\"\n")
		writeCodexConfig(t, tgt, "model = \"gpt-5\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt); key != "model_reasoning_effort" || inc {
			t.Fatalf("got (%q,%v), want (\"model_reasoning_effort\",false)", key, inc)
		}
	})
	t.Run("profile-resolved source compares effective values", func(t *testing.T) {
		writeCodexConfig(t, src, "profile = \"work\"\n[profiles.work]\nmodel = \"o3\"\n")
		writeCodexConfig(t, tgt, "model = \"o3\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt); key != "" || inc {
			t.Fatalf("got (%q,%v), want (\"\",false)", key, inc)
		}
	})
	t.Run("source key the target omits blocks even when effort also differs", func(t *testing.T) {
		writeCodexConfig(t, src, "model = \"gpt-5\"\nmodel_reasoning_effort = \"high\"\n")
		writeCodexConfig(t, tgt, "model_reasoning_effort = \"high\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt); key != "model" || inc {
			t.Fatalf("got (%q,%v), want (\"model\",false)", key, inc)
		}
	})
}
