package app

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/apiclient"
)

// mutationMayHaveLanded reports whether a failed TUI mutation may nonetheless
// have been applied by the daemon — apiclient.IsMutationOutcomeUncertain, which
// counts a committed-with-follow-up-failure outcome, a reply lost after the
// request may have been received, and an unverifiable reply (#4820).
//
// A caller that sees true must not treat the failure as a refusal: it does not
// re-arm the form that submitted the mutation (the user would re-submit a
// duplicate), it says the outcome is unknown, and it lets authoritative state —
// the snapshot and task polls, or an explicit re-read — show what happened.
// withDaemonHTTPMutation never re-sends such a mutation, so this is the only
// place its outcome is decided.
func mutationMayHaveLanded(err error) bool {
	return apiclient.IsMutationOutcomeUncertain(err)
}

// mutationOutcomeUnknown is mutationMayHaveLanded minus a committed outcome: the
// daemon may or may not have applied the mutation, and nothing says which. A
// call site that already handles a committed outcome as landed (archive,
// restore, handoff, delete project, …) keeps that branch and uses this for the
// rest, so a committed outcome behaves exactly as before (#4824).
func mutationOutcomeUnknown(err error) bool {
	return mutationMayHaveLanded(err) && !apiclient.IsMutationCommitted(err)
}

// mutationOutcomeError words a failure mutationMayHaveLanded accepted. action
// is the verb phrase ("creating session \"x\""); where names what the user
// should look at before retrying ("the sidebar"). A committed outcome is known
// to have landed and says so; anything else is unknown and says that instead,
// never "failed".
func mutationOutcomeError(action, where string, err error) error {
	if apiclient.IsMutationCommitted(err) {
		return fmt.Errorf("%s — done, with a warning: %w", action, err)
	}
	return fmt.Errorf("%s could not be confirmed — the daemon may have done it; check %s before trying again: %w", action, where, err)
}
