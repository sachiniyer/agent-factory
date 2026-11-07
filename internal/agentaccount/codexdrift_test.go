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
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "" || inc {
			t.Fatalf("got (%q,%v), want (\"\",false)", key, inc)
		}
	})
	t.Run("different model names the drift", func(t *testing.T) {
		writeCodexConfig(t, tgt, "model = \"gpt-5-mini\"\napproval_policy = \"on-request\"\nsandbox_mode = \"read-only\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "model" || inc {
			t.Fatalf("got (%q,%v), want (\"model\",false)", key, inc)
		}
	})
	t.Run("source key the target omits is drift", func(t *testing.T) {
		// No presence gap is presumed seedable: prep fills from ambient
		// prompts, not from source settings (#6817 review).
		writeCodexConfig(t, tgt, "approval_policy = \"on-request\"\nsandbox_mode = \"read-only\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "model" || inc {
			t.Fatalf("got (%q,%v), want (\"model\",false)", key, inc)
		}
	})
	t.Run("unparseable target is incomparable", func(t *testing.T) {
		writeCodexConfig(t, tgt, "not = [toml\n")
		if _, inc := CodexRuntimeDriftKey(src, tgt, "codex"); !inc {
			t.Fatalf("unparseable target was comparable")
		}
	})
	t.Run("missing target config is drift when source sets keys", func(t *testing.T) {
		// An absent file is provably empty, not unverifiable — and a source
		// that sets runtime keys cannot be presumed equal to an empty target.
		if err := os.Remove(filepath.Join(tgt, "config.toml")); err != nil {
			t.Fatal(err)
		}
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "model" || inc {
			t.Fatalf("got (%q,%v), want (\"model\",false)", key, inc)
		}
	})
	t.Run("empty source and empty target compare equal", func(t *testing.T) {
		writeCodexConfig(t, src, "")
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "" || inc {
			t.Fatalf("got (%q,%v), want (\"\",false)", key, inc)
		}
	})
	t.Run("source effort the target lacks is drift", func(t *testing.T) {
		writeCodexConfig(t, src, "model = \"gpt-5\"\nmodel_reasoning_effort = \"high\"\n")
		writeCodexConfig(t, tgt, "model = \"gpt-5\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "model_reasoning_effort" || inc {
			t.Fatalf("got (%q,%v), want (\"model_reasoning_effort\",false)", key, inc)
		}
	})
	t.Run("profile-resolved source compares effective values", func(t *testing.T) {
		writeCodexConfig(t, src, "profile = \"work\"\n[profiles.work]\nmodel = \"o3\"\n")
		writeCodexConfig(t, tgt, "model = \"o3\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "" || inc {
			t.Fatalf("got (%q,%v), want (\"\",false)", key, inc)
		}
	})
	t.Run("source key the target omits blocks even when effort also differs", func(t *testing.T) {
		writeCodexConfig(t, src, "model = \"gpt-5\"\nmodel_reasoning_effort = \"high\"\n")
		writeCodexConfig(t, tgt, "model_reasoning_effort = \"high\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "model" || inc {
			t.Fatalf("got (%q,%v), want (\"model\",false)", key, inc)
		}
	})
	t.Run("workspace-write options differ is drift", func(t *testing.T) {
		// #6817 review: sandbox_mode alone is not the sandbox — a move into
		// a home with network_access on would widen what the agent reaches.
		writeCodexConfig(t, src, "sandbox_mode = \"workspace-write\"\n[sandbox_workspace_write]\nnetwork_access = false\n")
		writeCodexConfig(t, tgt, "sandbox_mode = \"workspace-write\"\n[sandbox_workspace_write]\nnetwork_access = true\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "sandbox_workspace_write" || inc {
			t.Fatalf("got (%q,%v), want (\"sandbox_workspace_write\",false)", key, inc)
		}
	})
	t.Run("workspace-write options present on one side only is drift", func(t *testing.T) {
		writeCodexConfig(t, src, "sandbox_mode = \"workspace-write\"\n[sandbox_workspace_write]\nnetwork_access = true\n")
		writeCodexConfig(t, tgt, "sandbox_mode = \"workspace-write\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "sandbox_workspace_write" || inc {
			t.Fatalf("got (%q,%v), want (\"sandbox_workspace_write\",false)", key, inc)
		}
	})
	t.Run("read-only leftover table on one side is not drift", func(t *testing.T) {
		// sandbox_workspace_write is inert under read-only; a leftover table
		// (the seeder never strips it) must not be treated as an effective
		// setting, or a stale operator edit becomes a false config-drift edge.
		writeCodexConfig(t, src, "sandbox_mode = \"read-only\"\n[sandbox_workspace_write]\nnetwork_access = true\n")
		writeCodexConfig(t, tgt, "sandbox_mode = \"read-only\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "" || inc {
			t.Fatalf("got (%q,%v), want (\"\",false)", key, inc)
		}
	})
	t.Run("malformed inactive table is incomparable not silently equal", func(t *testing.T) {
		// codexRuntimeSource does not shape-validate the table outside
		// workspace-write, so a wrong-type value is not inert: the compare
		// rejects it as incomparable rather than treating the unvalidated
		// form as equal.
		writeCodexConfig(t, src, "sandbox_mode = \"read-only\"\nsandbox_workspace_write = \"nonsense\"\n")
		writeCodexConfig(t, tgt, "sandbox_mode = \"read-only\"\nsandbox_workspace_write = \"nonsense\"\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "sandbox_workspace_write" || !inc {
			t.Fatalf("got (%q,%v), want (\"sandbox_workspace_write\",true)", key, inc)
		}
	})
	t.Run("omitted sandbox mode with differing tables is drift not silently equal", func(t *testing.T) {
		// An omitted sandbox_mode may default to workspace-write upstream
		// (project trust), so a differing table is not presumed inert: the
		// compare catches it rather than silently widening the sandbox.
		writeCodexConfig(t, src, "[sandbox_workspace_write]\nnetwork_access = true\n")
		writeCodexConfig(t, tgt, "[sandbox_workspace_write]\nnetwork_access = false\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "sandbox_workspace_write" || inc {
			t.Fatalf("got (%q,%v), want (\"sandbox_workspace_write\",false)", key, inc)
		}
	})
	t.Run("cross-mode asymmetry drifts on sandbox_mode not workspace options", func(t *testing.T) {
		// source is workspace-write with a table, target is read-only with a
		// leftover table: the modes differ, so drift is reported on
		// sandbox_mode (compared earlier), never reaching the gated key.
		writeCodexConfig(t, src, "sandbox_mode = \"workspace-write\"\n[sandbox_workspace_write]\nnetwork_access = true\n")
		writeCodexConfig(t, tgt, "sandbox_mode = \"read-only\"\n[sandbox_workspace_write]\nnetwork_access = true\n")
		if key, inc := CodexRuntimeDriftKey(src, tgt, "codex"); key != "sandbox_mode" || inc {
			t.Fatalf("got (%q,%v), want (\"sandbox_mode\",false)", key, inc)
		}
	})
}

// TestCodexRuntimeDriftKeyProgramOverride is #6902 (#6919 review): the
// session's launch program can change the sandbox mode or the selected
// profile on the command line, so the config files' explicit non-workspace
// mode cannot settle whether sandbox_workspace_write is inert by itself.
// The program is CLASSIFIED, never interpreted: only a program provably
// built of the words af itself injects lets the configs decide; anything
// else compares the table — drift when it differs, equal when it does not —
// so a classification mistake can never skip a live table.
func TestCodexRuntimeDriftKeyProgramOverride(t *testing.T) {
	base := t.TempDir()
	src, tgt := filepath.Join(base, "src"), filepath.Join(base, "tgt")

	// Both homes set read-only yet disagree on the workspace table — the
	// exact trap the issue describes: under any mode the command line could
	// force, "inert" stops being true.
	writeBoth := func(srcTable, tgtTable string) {
		writeCodexConfig(t, src, "sandbox_mode = \"read-only\"\n"+srcTable)
		writeCodexConfig(t, tgt, "sandbox_mode = \"read-only\"\n"+tgtTable)
	}
	differing := func() {
		writeBoth("[sandbox_workspace_write]\nnetwork_access = false\n",
			"[sandbox_workspace_write]\nnetwork_access = true\n")
	}

	t.Run("programs built only of af's own words still allow the skip", func(t *testing.T) {
		for _, program := range []string{
			"codex",
			// The one -c pair af writes into every codex launch
			// (EnsureCodexUpdateCheckDisabled).
			"codex -c check_for_update_on_startup=false",
			"codex -c check_for_update_on_startup=false -c check_for_update_on_startup=false",
			// af's resume splices: " resume --last" and " resume <id>".
			"codex -c check_for_update_on_startup=false resume --last",
			"codex -c check_for_update_on_startup=false resume abc123",
			"codex -c check_for_update_on_startup=false resume abc123 'seed prompt'",
			// Supported subcommand launches carrying no other arguments.
			"codex exec 'run the suite'",
			"codex 'fix the tests'",
			// af's recorded wrapper spellings the launch boundary accepts.
			"CODEX_HOME=/tmp/h codex -c check_for_update_on_startup=false",
			"env CODEX_HOME=/tmp/h codex -c check_for_update_on_startup=false",
			"exec codex -c check_for_update_on_startup=false",
			"/opt/bin/codex -c check_for_update_on_startup=false",
			// Option parsing ends at --, so a flag-shaped word after it is a
			// positional prompt, not a sandbox option.
			"codex -- -s",
		} {
			differing()
			if key, inc := CodexRuntimeDriftKey(src, tgt, program); key != "" || inc {
				t.Fatalf("program %q: got (%q,%v), want (\"\",false)", program, key, inc)
			}
		}
	})

	t.Run("any non-af argument makes a differing table compare", func(t *testing.T) {
		for _, program := range []string{
			// Every sandbox-mode flag form a launch can carry.
			"codex -s workspace-write",
			"codex -s=workspace-write",
			"codex -sworkspace-write",
			"codex --sandbox workspace-write",
			"codex --sandbox=workspace-write",
			"codex --full-auto",
			"codex --approve-for-me",
			"codex --not-so-yolo",
			"codex --dangerously-bypass-approvals-and-sandbox",
			"codex --yolo",
			"codex -s read-only",
			"codex -s",
			"codex --sandbox",
			// A -c operand af did not write — including the whitespace key
			// variant codex itself accepts — is never provable.
			"codex -c 'sandbox_mode=\"workspace-write\"'",
			"codex -c sandbox_mode=workspace-write",
			"codex -c ' sandbox_mode = \"workspace-write\" '",
			"codex -c 'sandbox_mode=7'",
			"codex -c 'profile=\"work\"'",
			"codex -c 'profiles.work.sandbox_mode=\"read-only\"'",
			"codex -c not-toml",
			"codex -c ''",
			"codex -c",
			"codex -c foo",
			"codex -c check_for_update_on_startup=false -c 'model=o3'",
			// The byte-exact requirement applies to af's own word too: only
			// the separated spelling af writes is neutral.
			"codex -c=check_for_update_on_startup=false",
			"codex -ccheck_for_update_on_startup=false",
			"codex -c ' check_for_update_on_startup=false '",
			// Profile selection is per-home state.
			"codex -p work",
			"codex -pwork",
			"codex --profile=work",
			// af's -c pair plus anything else still cannot be proven.
			"codex -c check_for_update_on_startup=false -s workspace-write",
			"codex -c check_for_update_on_startup=false resume abc123 -s workspace-write",
			// Other option shapes af does not model.
			"codex --bogus-flag",
			"codex -i img.png",
			"codex -m gpt-5",
			"codex --json",
			"codex exec --json",
			"codex exec -s workspace-write",
			"codex -a never",
			"codex --unknown-after-subcommand exec -s workspace-write",
			// Subcommands af does not model and too many positionals.
			"codex login",
			"codex foo bar",
			"codex resume one two three",
			// Wrappers, compound commands, expansions, and nothing at all.
			"",
			"my-wrapper codex -s workspace-write",
			"ionice -c 3 codex -s workspace-write",
			"npx codex -s workspace-write",
			"bash -c 'codex -s workspace-write'",
			"codex -s workspace-write && echo done",
			"codex -s workspace-write | tee log",
		} {
			differing()
			key, inc := CodexRuntimeDriftKey(src, tgt, program)
			if key != "sandbox_workspace_write" || inc {
				t.Fatalf("program %q: got (%q,%v), want (\"sandbox_workspace_write\",false)", program, key, inc)
			}
		}
	})

	t.Run("equal tables stay equal when the program is not neutral", func(t *testing.T) {
		// Failing closed means comparing, not refusing: identical tables are
		// identical under any mode the command line could force.
		writeBoth("[sandbox_workspace_write]\nnetwork_access = true\n",
			"[sandbox_workspace_write]\nnetwork_access = true\n")
		for _, program := range []string{
			"codex -s workspace-write",
			"codex --full-auto",
			"",
			"my-wrapper codex",
		} {
			if key, inc := CodexRuntimeDriftKey(src, tgt, program); key != "" || inc {
				t.Fatalf("program %q: got (%q,%v), want (\"\",false)", program, key, inc)
			}
		}
	})

	t.Run("malformed table is incomparable even for a non-neutral program", func(t *testing.T) {
		// codexRuntimeSource validates the table only under workspace-write,
		// so a wrong-type value stays unverifiable whether or not the program
		// could have made it effective (#6875 semantics unchanged).
		writeBoth("sandbox_workspace_write = \"nonsense\"\n",
			"sandbox_workspace_write = \"nonsense\"\n")
		key, inc := CodexRuntimeDriftKey(src, tgt, "codex -s workspace-write")
		if key != "sandbox_workspace_write" || !inc {
			t.Fatalf("got (%q,%v), want (\"sandbox_workspace_write\",true)", key, inc)
		}
	})
}
