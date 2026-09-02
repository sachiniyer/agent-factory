// Notices that accompany a SUCCESSFUL session read rather than replace it: the
// "this answer is incomplete, and here is what I could not read" case.
//
// Split out of sessions.go, which holds the read FLOW; this holds the reporting
// rule that flow applies. Their own file because the rule is subtle enough to
// need explaining once, in one place, rather than inline at each caller.

package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// notedIncompleteAnswers dedupes incomplete-answer notices within one command
// invocation.
//
// `sessions watch` polls its getter every couple of seconds until the session is
// ready, and an unscoped watch resolves through the same widening every time —
// roughly 900 iterations at the default 30-minute timeout. Printed each pass,
// the caveat stops being a caveat and becomes a scrolling wall that buries
// whatever else the command says (#3479 review). Said once, it is still true.
var notedIncompleteAnswers sync.Map

// noteOnce writes msg to warnWriter the first time it is seen in this process,
// in text-output mode. Under --json this is not the only channel the notice
// travels through: warnIncompleteTitleWidening also returns it, so a JSON
// caller can carry it in its own payload instead (#3511).
func noteOnce(msg string) {
	if envelopeOutput {
		return
	}
	if _, seen := notedIncompleteAnswers.LoadOrStore(msg, struct{}{}); seen {
		return
	}
	fmt.Fprint(warnWriter, msg)
}

// warnIncompleteTitleWidening reports that the cross-project ambiguity widening
// behind an unscoped `sessions get` could not check every project, so the
// project it named may be the wrong one, and returns that same notice as a
// trimmed one-line string — empty when there is nothing to report.
//
// Both ways that check can come up short are reported, because both leave the
// same hole: per-repo records it could not read (gaps), and a failure to
// enumerate the instances directory at all (err). The enumeration failure is the
// WIDER outage of the two — nothing was checked rather than most things — so
// silently dropping it while reporting the narrower one would have been backwards.
//
// Reported rather than refused. The daemon's twin of this guard (findSession)
// does refuse, because the destructive paths resolve through it; this one
// backstops a read, where breaking a working lookup costs more than the wrong
// project name it would prevent (#3479).
//
// Text-output mode prints it once via noteOnce, deduped exactly as documented
// there. Under --json, stderr carries only the {data,error} envelope — a
// free-form line there hands automation invalid JSON on a successful command
// (#3169) — so noteOnce is a no-op there and the returned string is the only
// channel left. #3511 asked whether closing that hole needs a structural field
// on the shared apiproto.Envelope; for THIS notice it doesn't; the one caller
// that needs it (sessionsGetCmd) carries the returned string into its own
// command's `data` payload instead, which is additive to one command's JSON
// shape and needs no contract sign-off. That leaves #3511's broader question —
// other commands, and the envelope itself — open for Sachin.
func warnIncompleteTitleWidening(title string, gaps []config.RepoInstancesSkip, err error) string {
	var msg string
	switch {
	case err != nil:
		msg = fmt.Sprintf(
			"warning: could not check any project for another session titled %q, so this result may name the wrong project: %v; pass --repo <path> to scope the lookup\n",
			title, err)
	case len(gaps) > 0:
		msg = fmt.Sprintf(
			"warning: could not check every project for another session titled %q, so this result may name the wrong project: %s; pass --repo <path> to scope the lookup, or %s\n",
			title, config.DescribeRepoInstancesSkips(gaps), config.RepoInstancesSkipRemedy(gaps))
	default:
		return ""
	}
	noteOnce(msg)
	return strings.TrimRight(msg, "\n")
}

// sessionGetResult is `sessions get`'s --json payload shape when the
// cross-project ambiguity widening behind it came up short (#3511). Embedding
// InstanceData keeps every field an existing --json consumer decodes at the
// same top level; Warnings is new and omitempty, so decoding only the fields a
// client already knows is unaffected. In text-output mode the same notice
// prints to stderr instead (see warnIncompleteTitleWidening above) — stderr
// under --json is reserved for the {data,error} envelope alone (#3169), which
// is the hole this type closes without touching that envelope.
type sessionGetResult struct {
	session.InstanceData
	Warnings []string `json:"warnings,omitempty"`
}

// MarshalJSON encodes the session and then splices Warnings in beside it.
//
// The embedding above would otherwise be silently lossy: InstanceData carries
// its own MarshalJSON (it derives status_name/liveness_name at encode time,
// #3631), that method is PROMOTED to this struct, and encoding/json calls the
// promoted method for the whole value — emitting a bare session object and
// dropping `warnings` entirely. Delegating explicitly restores the flat shape
// this type exists for, with both halves present.
//
// The splice preserves the session's field order rather than re-encoding through
// a map, which would alphabetize every key of a public payload.
func (r sessionGetResult) MarshalJSON() ([]byte, error) {
	object, err := json.Marshal(r.InstanceData)
	if err != nil {
		return nil, err
	}
	if len(r.Warnings) == 0 {
		return object, nil
	}
	warnings, err := json.Marshal(r.Warnings)
	if err != nil {
		return nil, err
	}
	return session.AppendJSONMember(object, "warnings", warnings)
}

// sessionGetPayload builds sessionsGetCmd's --json payload for a resolved
// session and its ambiguity-widening notice (empty when there is nothing to
// report). Split out so the embedding decision is testable without cobra's
// RunE plumbing.
func sessionGetPayload(data *session.InstanceData, notice string) any {
	if notice == "" {
		return data
	}
	return sessionGetResult{InstanceData: *data, Warnings: []string{notice}}
}
