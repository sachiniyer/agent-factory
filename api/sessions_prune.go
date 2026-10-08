package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
)

var (
	sessionsPruneAllFlag      bool
	sessionsPruneOlderThanStr string
)

// `af sessions prune` is the manual, opt-in reclaim for archived sessions
// (#5136): the archive keeps every shelved worktree forever, and the fleet
// that motivated it measured ~146G under the archive root. Slice 1 is the
// READ-ONLY half — this command only ever runs the dry-run listing; it
// reports which archived sessions a future --apply would reclaim, what each
// would free, and why the rest refuse. The apply half — deletion plus the
// tombstone record that keeps the row listed — lands in the follow-up issue
// referenced on the PR, and carries the guard stack this listing previews.
var sessionsPruneCmd = &cobra.Command{
	Use:   "prune --older-than <duration> [--repo <path> | --all]",
	Short: "List disk reclaimable from old archived sessions (dry run)",
	Long: `Report archived sessions older than --older-than, measured from each
session's archive time.

This is strictly a dry run: it lists each archived session a reclaim would
remove (title, archive time, allocated bytes) plus a total, and the reasons
every other session was skipped. It changes nothing — no deletion, no record
update, no git mutation — so it is safe to run at any time.

Only archived sessions are eligible; live, lost, dead or in-flight sessions,
archives whose move is incomplete, and worktrees still holding uncommitted or
ignored files are skipped with their reasons. A session whose origin
repository is gone is refused too: the archived tree may be the work's last
copy, so it is reported, not counted reclaimable.

Bytes are ALLOCATED disk space (what rm -rf would free), not apparent file
size — sparse holes and hard links whose other end lives outside the tree are
not counted.

The listing reads the running daemon's in-flight claims and will not spawn
one — a read must not write even a socket or a log. With no daemon up it
refuses with guidance; pass --daemon-url or start af first.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// No log.Initialize: it creates/rotates the AF log file, and a
		// strictly read-only listing must not write anything — not even a
		// log (#5136 Codex round 6). The package's discard loggers already
		// satisfy anything that logs along the way.
		if sessionsPruneAllFlag && repoFlag != "" {
			return jsonError(fmt.Errorf("--repo and --all are mutually exclusive: --repo names one project, --all spans every project"))
		}
		if strings.TrimSpace(sessionsPruneOlderThanStr) == "" {
			return jsonError(fmt.Errorf("--older-than is required: prune is deliberately opt-in and has no default retention period"))
		}

		var repoID string
		if !sessionsPruneAllFlag {
			if apiclient.IsRemoteTarget() {
				// A repo scope is resolved by hashing a checkout path on THIS
				// machine — against a remote daemon it can name a different
				// project entirely (whatever the remote happens to keep at
				// that path). Require the honest scope (#5136 Codex round 5).
				return jsonError(fmt.Errorf("repo scoping (--repo, the current directory) resolves against this machine's checkouts, which a remote --daemon-url cannot honor — pass --all to list the remote daemon's archives"))
			}
			var err error
			repoID, err = resolveRepoID()
			if err != nil {
				return jsonError(err)
			}
			if repoID == "" {
				return jsonError(fmt.Errorf("no project context: run inside a git repository, or pass --repo <path> for one project or --all for every project"))
			}
		}

		resp, err := pruneSessionsViaDaemon(daemon.PruneSessionsRequest{
			RepoID:    repoID,
			All:       sessionsPruneAllFlag,
			OlderThan: sessionsPruneOlderThanStr,
		})
		if err != nil {
			if errors.Is(err, daemon.ErrDaemonUnavailable) {
				// A read must not launch a daemon — the socket dial alone is
				// what stays read-only. The listing needs the daemon's
				// in-flight claims, so disk alone cannot serve it.
				return jsonError(fmt.Errorf("no daemon is running — the prune listing reads its in-flight claims, so it cannot be served from disk alone; start one (running af or another af command launches it), or pass --daemon-url"))
			}
			return jsonError(err)
		}
		return jsonOut(resp)
	},
}
