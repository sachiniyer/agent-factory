package api

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/session"
)

// reportCreatedSession keeps creation and prompt observation distinct. The
// sender already records its verdict on the returned row, but its error-only
// startup wrapper accepts unverified sends. A created runtime is not evidence
// that --prompt started work (#4200). Never retry an ambiguous submission here.
func reportCreatedSession(data *session.InstanceData, prompt string) error {
	if prompt == "" || data.Liveness == session.LiveLimitReached || data.LastPromptDeliveryStatus == session.PromptDelivered {
		return jsonOut(data)
	}
	status := data.LastPromptDeliveryStatus
	if !status.Valid() {
		status = session.PromptCouldNotConfirm
	}
	warning := fmt.Sprintf("Session %q was created, but initial prompt submission is unconfirmed (delivery status: %s). Inspect its pane before retrying; the prompt may already have run or may still be in the composer. Do not recreate the session.", data.Title, status)
	if status == session.PromptNotDelivered {
		warning = fmt.Sprintf("Session %q was created, but its initial prompt was observed not fully delivered. Inspect its pane before retrying; Enter was sent best-effort. Do not recreate the session.", data.Title)
	}
	// Preserve InstanceData's public JSON projection (including named states),
	// and its exact numbers, while adding the warning to the existing flat row.
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	result["warning"], err = json.Marshal(warning)
	if err != nil {
		return err
	}
	if !envelopeOutput {
		fmt.Fprintln(os.Stderr, "Warning: "+warning)
	}
	return jsonOut(result)
}
