package daemon

import (
	"slices"

	"github.com/sachiniyer/agent-factory/config"
)

// completeConfigSaveWarnings keeps the response-level warning list complete
// when a live apply fails or its reply is lost. In those states ApplyConfig did
// not return its normal warning set, so the write-time warnings on Result are
// not superseded and must travel beside the apply error for response renderers.
//
// A successful apply keeps its existing authoritative warning set. In
// particular, both the writer and Manager.ApplyConfig describe tokenless
// listener exposure in different words; merging those would show one hazard
// twice on every successful save.
func completeConfigSaveWarnings(outcome config.ApplyOutcome, writeWarnings, applyWarnings []string) []string {
	if !outcome.DaemonApplyFailed && !outcome.DaemonApplyUnconfirmed {
		return applyWarnings
	}
	warnings := append([]string(nil), writeWarnings...)
	for _, warning := range applyWarnings {
		if !slices.Contains(warnings, warning) {
			warnings = append(warnings, warning)
		}
	}
	return warnings
}
