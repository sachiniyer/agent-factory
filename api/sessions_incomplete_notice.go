// Notices that accompany a SUCCESSFUL session read rather than replace it: the
// "this answer is incomplete, and here is what I could not read" case.
//
// Split out of sessions.go, which holds the read FLOW; this holds the reporting
// rule that flow applies. Their own file because the rule is subtle enough to
// need explaining once, in one place, rather than inline at each caller.

package api

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// warnIncompleteTitleWidening reports that the cross-project ambiguity widening
// behind an unscoped `sessions get` could not read every project's records, so
// the project it named may be the wrong one.
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
func warnIncompleteTitleWidening(title string, gaps []config.RepoInstancesSkip) {
	if len(gaps) == 0 || envelopeOutput {
		return
	}
	fmt.Fprintf(warnWriter,
		"warning: could not check every project for another session titled %q, so this result may name the wrong project: %s; pass --repo <path> to scope the lookup, or repair or remove the file(s)\n",
		title, config.DescribeRepoInstancesSkips(gaps))
}
