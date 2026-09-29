package task

import (
	"strconv"
	"strings"
)

// MaxCapValue is the largest concurrency cap a UI surface may set: the largest
// integer JavaScript represents exactly (Number.MAX_SAFE_INTEGER). Above it the
// web form would parse the same digits to a different value than the Go
// surfaces — the two-UIs-disagree defect #4180 exists to close — so every
// surface refuses it rather than disagree. The bound is representational, not
// policy: no machine runs anywhere near a quadrillion sessions at once, so it
// excludes no sensible cap.
const MaxCapValue = 9007199254740991

// CapApplies is THE one predicate for "can this task shape carry a concurrency
// cap" (#4180). Every surface asks it — the TUI form, the daemon's
// ValidateTrigger, and (via its pinned web twin, web/src/tasks.ts
// capUnavailableReason) the browser — rather than re-deriving the rule.
//
// The cap bounds sessions a watch task spawns per event, so it is meaningful
// only for a watch task that creates them (#1892). Cron fires already coalesce
// on RunTask's lock, and target-session deliveries already serialize into one
// session, so neither shape has anything for a cap to bound.
//
// isWatch is the caller's own ground truth for "this is a watch task": the
// store asks (Task).IsWatch on the record; a form asks its trigger selector.
// The TargetSession test goes through CanonicalTargetSession, the same
// function deliverTaskPrompt's runtime "create a session per event" condition
// uses — the two must agree on what an empty target session is, or a cap could
// validate against one condition and be bypassed at delivery by the other.
func CapApplies(isWatch bool, targetSession string) bool {
	return CapUnavailableReason(isWatch, targetSession) == ""
}

// CapUnavailableReason says WHY a task shape cannot carry a concurrency cap,
// or "" when it can. Both UIs render it in place of the input on an
// inapplicable shape (#4180) — the strings are the shared refusal contract,
// pinned by testdata/cap_vectors.json so the surfaces cannot drift, and they
// say what the daemon's own validator says (ValidateTrigger). The check order
// is part of the contract: a shape failing both rules reports the trigger
// reason.
func CapUnavailableReason(isWatch bool, targetSession string) string {
	if !isWatch {
		return "Cron fires already coalesce."
	}
	if CanonicalTargetSession(targetSession) != "" {
		return "Deliveries into one session already serialize."
	}
	return ""
}

// ParseCapInput parses a UI-typed cap into the stored int: empty means 0 — the
// default, unlimited — and anything else must be a non-negative integer within
// MaxCapValue. The web twin (web/src/tasks.ts parseCapInput) is pinned to the
// same answers by testdata/cap_vectors.json, so the two forms refuse and accept
// the same strings.
func ParseCapInput(raw string) (int, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > MaxCapValue {
		return 0, false
	}
	return n, true
}
