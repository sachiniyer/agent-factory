package api

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

var (
	sessionsPruneAllFlag      bool
	sessionsPruneApplyFlag    bool
	sessionsPruneOlderThanStr string
)

// `af sessions prune` is the manual, opt-in reclaim for archived sessions
// (#5136): the archive keeps every shelved worktree and provider capture
// forever, and the fleet that motivated it measured ~146G under the archive
// root. The command is deliberately NOT a retention policy — it deletes only
// what an explicit --apply asks for, keeps each session's branch and a small
// tombstone record, and leaves restore to refuse with a pointer back to the
// branch rather than a silent failure.
var sessionsPruneCmd = &cobra.Command{
	Use:   "prune --older-than <duration> [--repo <path> | --all] [--apply]",
	Short: "Reclaim disk from old archived sessions",
	Long: `Prune archived sessions older than --older-than, measured from each
session's archive time.

Without --apply this is a dry run: it lists each archived session it would
prune (title, archive time, bytes reclaimed) plus a total, and changes
nothing. With --apply it deletes those sessions' archived worktrees and
provider conversation captures, runs 'git worktree prune' for the repo, and
tombstones each record — the row stays listed in 'af sessions list --all'
with its title, branch, archive time and prune time.

Pruning never deletes a branch. 'af sessions restore' on a pruned session
refuses and names the kept branch, so the work can be recreated from it.
Only archived sessions are eligible; live, lost, dead or in-flight sessions,
and archives whose move is incomplete, are skipped with their reasons.

--apply asks for confirmation when stdin is a terminal; in scripts and pipes
it proceeds on the flag alone.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if sessionsPruneAllFlag && repoFlag != "" {
			return jsonError(fmt.Errorf("--repo and --all are mutually exclusive: --repo names one project, --all spans every project"))
		}
		if strings.TrimSpace(sessionsPruneOlderThanStr) == "" {
			return jsonError(fmt.Errorf("--older-than is required: prune is deliberately opt-in and has no default retention period"))
		}

		var repoID string
		if !sessionsPruneAllFlag {
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

		// The plan is always a dry run first: it validates --older-than, lists
		// the candidates, and — on a TTY apply — is what the confirmation
		// prompt summarizes. Script callers skip the prompt but still get the
		// same two-phase truth: what apply reports is what apply deleted.
		plan, err := pruneSessionsViaDaemon(req)
		if err != nil {
			return jsonError(err)
		}
		if !sessionsPruneApplyFlag {
			return jsonOut(plan)
		}

		if term.IsTerminal(int(os.Stdin.Fd())) {
			ok, err := confirmPruneApply(cmd.ErrOrStderr(), os.Stdin, plan)
			if err != nil {
				return jsonError(err)
			}
			if !ok {
				return jsonOut(map[string]any{"ok": false, "aborted": true, "applied": false})
			}
		}
		req.Apply = true
		resp, err := pruneSessionsViaDaemon(req)
		if err != nil && !apiclient.IsMutationCommitted(err) {
			return jsonError(err)
		}
		return jsonOut(resp)
	},
}

// confirmPruneApply renders the dry-run plan on the error stream — never
// stdout, which carries the JSON payload — and asks once. Anything but y/yes
// is a refusal.
func confirmPruneApply(out io.Writer, in io.Reader, plan daemon.PruneSessionsResponse) (bool, error) {
	fmt.Fprintf(out, "Prune %d archived session(s), reclaiming ~%s:\n", len(plan.Pruned), formatBytes(plan.ReclaimedBytes))
	for _, entry := range plan.Pruned {
		fmt.Fprintf(out, "  %s (%s, %s, %s archived %s)\n",
			entry.Title, entry.RepoID, entry.Branch, formatBytes(entry.ReclaimedBytes), entry.ArchivedAt.Format("2006-01-02"))
	}
	if len(plan.Skipped) > 0 {
		fmt.Fprintf(out, "%d session(s) skipped (see dry-run output for reasons).\n", len(plan.Skipped))
	}
	fmt.Fprint(out, "Delete their archived worktrees and conversation captures? Branches and tombstone records are kept. [y/N] ")
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

// formatBytes renders a byte count for the confirmation summary; the JSON
// payload always carries the exact int64, so this only needs to be readable.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
