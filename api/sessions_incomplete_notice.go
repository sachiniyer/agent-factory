// Notices that accompany a SUCCESSFUL session read rather than replace it: the
// "this answer is incomplete, and here is what I could not read" case.
//
// Split out of sessions.go, which holds the read FLOW; this holds the reporting
// rule that flow applies. Their own file because the rule is subtle enough to
// need explaining once, in one place, rather than inline at each caller.

package api

import (
	"fmt"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
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

// noteOnce writes msg to warnWriter the first time it is seen in this process.
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
// project it named may be the wrong one.
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
// Silent under --json, which promises stderr carries the {data,error} envelope —
// a free-form line there hands automation invalid JSON on a successful command
// (#3169). That leaves the mode most likely to be scripted without the caveat;
// closing it needs a structural channel in the envelope, which is a public API
// contract change and is tracked in #3511.
func warnIncompleteTitleWidening(title string, gaps []config.RepoInstancesSkip, err error) {
	switch {
	case err != nil:
		noteOnce(fmt.Sprintf(
			"warning: could not check any project for another session titled %q, so this result may name the wrong project: %v; pass --repo <path> to scope the lookup\n",
			title, err))
	case len(gaps) > 0:
		noteOnce(fmt.Sprintf(
			"warning: could not check every project for another session titled %q, so this result may name the wrong project: %s; pass --repo <path> to scope the lookup, or %s\n",
			title, config.DescribeRepoInstancesSkips(gaps), config.RepoInstancesSkipRemedy(gaps)))
	}
}
