package agentaccount

import (
	"path/filepath"
	"reflect"
)

// codexRuntimeCompareKeys are the effective settings a moved session must not
// change underneath it (design R9): model, effort, approval, sandbox. The
// comparison is profile-resolved — codexEffectiveSettings already overlays the
// selected profile — and values only ever compare, never log or copy.
var codexRuntimeCompareKeys = []string{"model", "model_reasoning_effort", "approval_policy", "sandbox_mode"}

// codexRuntimeSeedableKeys are the compare keys target-home preparation can
// seed when the target lacks them — the set answerCodexLoginPrompts fills.
// model_reasoning_effort is absent on purpose: no prep copies it yet, so a
// missing effort on the target is a real difference, not a seedable gap.
var codexRuntimeSeedableKeys = map[string]bool{
	"model":           true,
	"approval_policy": true,
	"sandbox_mode":    true,
}

// CodexRuntimeDriftKey compares the effective runtime settings of a source
// home (the session's own account dir, or the ambient home) against a
// candidate account home. It answers the one question the engine's candidate
// filter needs: does moving to target keep the session's model, effort,
// approval, and sandbox — or is the candidate ineligible?
//
// (key, false): the candidate is ineligible — it sets a different value, or
// omits a key the source sets that prep cannot seed.
// ("", false): comparable and equal (missing keys prep would seed don't count).
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
		if !tOK {
			// Target lacks the key: equal when prep can seed it from the
			// source, drift when it cannot (effort has no seeder today).
			if sOK && !codexRuntimeSeedableKeys[k] {
				return k, false
			}
			continue
		}
		if !sOK || !reflect.DeepEqual(sv, tv) {
			return k, false
		}
	}
	return "", false
}
