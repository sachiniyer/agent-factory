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
// what the agent can reach (#6817 review). The table is shape-validated by
// codexRuntimeSource before it reaches this compare, so an unverifiable form
// is incomparable, never silently equal.
var codexRuntimeCompareKeys = []string{"model", "model_reasoning_effort", "approval_policy", "sandbox_mode", "sandbox_workspace_write"}

// CodexRuntimeDriftKey compares the effective runtime settings of a source
// home (the session's own account dir, or the ambient home) against a
// candidate account home. It answers the one question the engine's candidate
// filter needs: does moving to target keep the session's model, effort,
// approval, and sandbox — or is the candidate ineligible?
//
// (key, false): the candidate is ineligible — it sets a different value, or
// sets/omits a key asymmetrically. No presence gap is presumed harmless:
// target-home preparation seeds from the ambient login prompts, not from the
// source's settings, so a source key the target omits is a real difference in
// effective runtime, not a seedable gap.
// ("", false): comparable and equal.
// ("", true): unevaluable — an unreadable or unparseable config on either
// side, or a doc whose runtime cannot be verified. Unevaluable also makes the
// candidate ineligible; the caller names the reason.
//
// No credential material, profile, or provider configuration is compared or
// copied — the same boundary the registration seeder holds.
func CodexRuntimeDriftKey(sourceDir, targetDir string) (key string, incomparable bool) {
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
		sv, sOK := source[k]
		tv, tOK := target[k]
		if sOK != tOK || (sOK && !reflect.DeepEqual(sv, tv)) {
			return k, false
		}
	}
	return "", false
}
