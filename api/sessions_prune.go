package api

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
)

var (
	sessionsPruneAllFlag      bool
	sessionsPruneOlderThanStr string
	sessionsPruneApplyFlag    bool
	sessionsPruneYesFlag      bool

	// pruneStdinIsTTY and pruneConfirmIn are seams for the confirmation prompt.
	pruneStdinIsTTY           = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	pruneConfirmIn  io.Reader = os.Stdin
)

// `af sessions prune` is the manual, opt-in reclaim for archived sessions
// (#5136): the archive keeps every shelved worktree forever, and the fleet
// that motivated it measured ~146G under the archive root. Without --apply it
// runs only the dry-run listing — which archived sessions an apply would
// reclaim, what each would free, and why the rest refuse. With --apply the
// listing becomes the confirmed plan: applyPrunePlan asks the user to approve
// it (--yes non-interactively), binds the apply to exactly those session IDs,
// and the daemon deletes each worktree, leaving a tombstone record that keeps
// the row listed (#5189).
var sessionsPruneCmd = &cobra.Command{
	Use:   "prune --older-than <duration> [--repo <path> | --all] [--apply [--yes]]",
	Short: "List (or with --apply, delete) old archived worktrees",
	Long: `Report archived sessions older than --older-than, measured from each
session's archive time.

Without --apply this is a dry run: it lists each archived session a reclaim
would remove (title, archive time, allocated bytes) plus a total, and the
reasons every other session was skipped. af writes nothing — no deletion, no
record update, no git mutation — so it is safe to run at any time.

With --apply, af prints that plan, asks you to confirm it (or pass --yes; a
non-interactive run requires it), and then deletes exactly the worktrees the
plan listed — the SAME judgment as the dry run, re-made under each session's
lock just before it deletes. The branch is always kept and the session row
stays, marked pruned, naming the branch; restore refuses a pruned session and
tells you where the work is. A refusal names the first offending paths so you
can clean up and re-run. Nothing outside the archived worktree is touched:
provider transcripts and other sessions' registrations are left alone.

Only archived sessions are eligible; live, lost, dead or in-flight sessions,
archives whose move is incomplete, and worktrees still holding uncommitted or
ignored files are skipped with their reasons. A session whose origin
repository is gone is refused too: the archived tree may be the work's last
copy, so it is reported, not counted reclaimable.

Bytes are ALLOCATED disk space, not apparent file size — sparse holes and
hard links whose other end lives outside the tree are not counted. It is an
upper bound on what a reclaim frees: extents shared with a copy-on-write
reflink clone outside the tree are still counted, so on Btrfs/XFS the free
space recovered can be less than the total.

The listing reads the running daemon's in-flight claims and will not spawn
one — a read must not write even a socket or a log. With no daemon up it
refuses with guidance; pass --daemon-url or start af first.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// No log.Initialize: it creates/rotates the AF log file, and this
		// listing must not write anything — not even a log (#5136 Codex
		// round 6). The package's discard loggers already satisfy anything
		// that logs along the way.
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

		req := daemon.PruneSessionsRequest{
			RepoID:    repoID,
			All:       sessionsPruneAllFlag,
			OlderThan: sessionsPruneOlderThanStr,
		}
		resp, err := pruneSessionsViaDaemon(req)
		if err == nil && sessionsPruneApplyFlag {
			resp, err = applyPrunePlan(cmd, req, resp)
		}
		if err != nil {
			if errors.Is(err, daemon.ErrDaemonUnavailable) {
				// A read must not launch a daemon — the socket dial alone is
				// what stays read-only. The listing needs the daemon's
				// in-flight claims, so disk alone cannot serve it.
				return jsonError(fmt.Errorf("no daemon is running — the prune listing reads its in-flight claims, so it cannot be served from disk alone; start one (running af or another af command launches it), or pass --daemon-url"))
			}
			return jsonError(err)
		}
		if err := jsonOut(resp); err != nil {
			return err
		}
		if n := len(resp.Incomplete); n > 0 {
			// The structured result above is the answer; the exit status reports
			// that some sessions are only partly pruned (re-run to finish them).
			silenceCobraOutput()
			return fmt.Errorf("%d session(s) were only partly pruned; re-run `af sessions prune --apply` to finish them", n)
		}
		return nil
	},
}

// applyPrunePlan turns the dry run just made into a confirmed apply: the user
// sees the plan, confirms it, and the apply is bound to exactly the sessions it
// listed (Only) — never re-scanned into something larger.
func applyPrunePlan(cmd *cobra.Command, req daemon.PruneSessionsRequest, plan daemon.PruneSessionsResponse) (daemon.PruneSessionsResponse, error) {
	// An interrupted prune is part of the plan: apply finishes it from its durable
	// intent, whatever age the row now reads as.
	var interrupted []daemon.PruneSkippedEntry
	for _, sk := range plan.Skipped {
		if sk.Interrupted {
			interrupted = append(interrupted, sk)
		}
	}
	if len(plan.Candidates) == 0 && len(interrupted) == 0 {
		// Nothing to delete: report the plan (and why every session was skipped)
		// without asking for a confirmation that would guard nothing.
		return plan, nil
	}
	if !sessionsPruneYesFlag {
		if !pruneStdinIsTTY() {
			return daemon.PruneSessionsResponse{}, fmt.Errorf("--apply deletes archived worktrees and needs a confirmation: run it in a terminal, or pass --yes to confirm the plan non-interactively")
		}
		out := cmd.ErrOrStderr()
		_, _ = fmt.Fprintf(out, "Plan: delete %d archived worktree(s), up to %d byte(s); each branch is kept.\n", len(plan.Candidates), plan.ReclaimableBytes)
		for _, c := range plan.Candidates {
			_, _ = fmt.Fprintf(out, "  %s  (branch %s, archived %s)\n", c.Title, c.Branch, c.ArchivedAt.Format("2006-01-02"))
		}
		for _, sk := range interrupted {
			_, _ = fmt.Fprintf(out, "  %s  (finish an interrupted prune)\n", sk.Title)
		}
		_, _ = fmt.Fprint(out, "Delete them? [y/N] ")
		line, _ := bufio.NewReader(pruneConfirmIn).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return daemon.PruneSessionsResponse{}, fmt.Errorf("not confirmed; nothing was deleted")
		}
	}
	// The apply is bound to exactly the plan the user saw: the sessions it listed
	// plus any interrupted prune it will finish.
	var only []string
	for _, c := range plan.Candidates {
		only = append(only, c.ID)
	}
	for _, sk := range interrupted {
		only = append(only, sk.ID)
	}
	return pruneSessionsViaDaemon(daemon.PruneSessionsRequest{
		RepoID: req.RepoID, All: req.All, OlderThan: req.OlderThan,
		Apply: true, Only: only,
	})
}
