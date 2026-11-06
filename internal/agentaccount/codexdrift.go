package agentaccount

import (
	"path/filepath"
	"reflect"
)

// codexRuntimeCompareKeys are the effective settings a moved session must not
// change underneath it (design R9): model, effort, approval, sandbox — and the
// sandbox's own options. Two homes that both say sandbox_mode =
// "workspace-write" but differ on sandbox_workspace_write (network_access,
// writable_roots, …) are not equivalent: a move to the wider one would widen
// what the agent can reach (#6817 review). sandbox_workspace_write is only an
// effective setting under workspace-write: codexRuntimeSource shape-validates
// the table only in that mode, and the seeder never strips an operator's
// leftover [sandbox_workspace_write] table when the mode changes, so in any
// other mode a stale table is inert. Comparing it there would report drift on
// a non-effective option and let an unvalidated form reach DeepEqual. The
// compare therefore skips the key only when both sides are explicitly in a
// non-workspace mode; an omitted sandbox_mode is not presumed inert (upstream
// Codex may default it to workspace-write under project trust), and a
// malformed inactive table is incomparable here because codexRuntimeSource
// does not validate it outside workspace-write. Any cross-mode asymmetry is
// already caught by the sandbox_mode key earlier in this list, so this gate
// cannot hide a widening.
var codexRuntimeCompareKeys = []string{"model", "model_reasoning_effort", "approval_policy", "sandbox_mode", "sandbox_workspace_write"}

// CodexRuntimeDriftKey compares the effective runtime settings of a source
// home (the session's own account dir, or the ambient home) against a
// candidate account home. program is the session's recorded launch command —
// a command-line sandbox override in it applies before config.toml's
// sandbox_mode, so the mode that will actually take effect is not the config's
// alone (#6902). It answers the one question the engine's candidate filter
// needs: does moving to target keep the session's model, effort, approval,
// and sandbox — or is the candidate ineligible?
//
// (key, false): the candidate is ineligible — it sets a different value, or
// sets/omits a key asymmetrically. No presence gap is presumed harmless:
// target-home preparation seeds from the ambient login prompts, not from the
// source's settings, so a source key the target omits is a real difference in
// effective runtime, not a seedable gap.
// ("", false): comparable and equal.
// ("", true): unevaluable — an unreadable or unparseable config on either
// side, a doc whose runtime cannot be verified, or a program whose effective
// sandbox mode cannot be proven. Unevaluable also makes the candidate
// ineligible; the caller names the reason.
//
// No credential material, profile, or provider configuration is compared or
// copied — the same boundary the registration seeder holds.
func CodexRuntimeDriftKey(sourceDir, targetDir, program string) (key string, incomparable bool) {
	_, sourceDoc, err := readCodexSettings(filepath.Join(sourceDir, "config.toml"))
	if err != nil || sourceDoc == nil {
		return "", true
	}
	_, targetDoc, err := readCodexSettings(filepath.Join(targetDir, "config.toml"))
	if err != nil || targetDoc == nil {
		return "", true
	}
	source, reason := codexRuntimeSource(sourceDoc)
	if reason != "" {
		return "", true
	}
	target, reason := codexRuntimeSource(targetDoc)
	if reason != "" {
		return "", true
	}
	for _, k := range codexRuntimeCompareKeys {
		// sandbox_workspace_write is only effective under workspace-write.
		// codexRuntimeSource shape-validates the table only in that mode, and
		// the seeder never strips an operator's leftover [sandbox_workspace_write]
		// when the mode changes, so in another mode a stale table is inert.
		// The compare skips the key only when both sides are explicitly in a
		// non-workspace mode: an omitted sandbox_mode is not presumed inert,
		// because upstream Codex may default it to workspace-write (project
		// trust), and silently skipping a differing table there could widen
		// the sandbox. Because codexRuntimeSource does not validate the table
		// outside workspace-write, a malformed inactive table (e.g. a
		// wrong-type value) is incomparable here, never silently equal.
		// sandbox_mode is compared earlier in this list, so a cross-mode
		// widening is already caught and cannot be hidden by this gate.
		if k == "sandbox_workspace_write" {
			sm, _ := source["sandbox_mode"].(string)
			tm, _ := target["sandbox_mode"].(string)
			if sm != "workspace-write" {
				if opts, exists := source["sandbox_workspace_write"]; exists && !validCodexWorkspaceOptions(opts) {
					return k, true
				}
			}
			if tm != "workspace-write" {
				if opts, exists := target["sandbox_workspace_write"]; exists && !validCodexWorkspaceOptions(opts) {
					return k, true
				}
			}
			if sm != "" && sm != "workspace-write" && tm != "" && tm != "workspace-write" {
				continue
			}
		}
		sv, sOK := source[k]
		tv, tOK := target[k]
		if sOK != tOK || (sOK && !reflect.DeepEqual(sv, tv)) {
			return k, false
		}
	}
	return "", false
}
