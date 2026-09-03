package api

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// The three session-lifecycle verbs — kill, archive, restore — moved out of
// api/sessions.go, which had grown to the file-length limit (#1145) as the
// package's residual grab-bag. This follows the split the rest of the package
// already uses (sessions_backends.go, sessions_handoff.go, sessions_watch.go):
// one file per command cluster. Their daemon seams stay in api/sessions.go with
// the other session verbs, so tests substitute them from one place.

var sessionsKillCmd = &cobra.Command{
	Use:   "kill <title>",
	Short: "Permanently destroy a session and prune its worktree branch",
	Long: `Permanently destroy a session: tear down tmux, remove the worktree,
delete the stored session record, and prune the session branch when Agent
Factory owns it.

For normal "done with this session" cleanup, prefer:
  af sessions archive <title>

Kill always destroys the session, including any uncommitted or unmerged work on
its branch — there is no undo. To keep a session restorable instead, archive it.
--force is accepted but has no effect (kept for backward compatibility).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// Honor --repo scoping (#761). An empty repoID preserves the prior
		// all-repo search; a non-empty one confines the kill to that repo so a
		// same-titled session in a different repo is never destroyed by
		// mistake. Mirrors sessionsListCmd's resolveRepoID() usage.
		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		// --force is accepted for backward compatibility but
		// is a no-op: it is intentionally NOT forwarded to the daemon, whose
		// KillSessionRequest no longer carries a force field (#1579).
		if err := killSessionViaDaemon(daemon.KillSessionRequest{Title: args[0], RepoID: repoID}); err != nil {
			return jsonError(err)
		}

		return jsonOut(map[string]bool{"ok": true})
	},
}

var sessionsArchiveCmd = &cobra.Command{
	Use:   "archive [title]",
	Short: "Finish with a session by archiving it for later restore",
	Long: `Archive is the default way to finish with a session: tear down its tmux
and move its git worktree out to the global archive directory
(<AGENT_FACTORY_HOME>/archived/<repoID>/<title>/), preserving the branch and any
uncommitted changes. The session is not deleted — it becomes a quiescent
"archived" row that survives restarts and can be brought back later with
'af sessions restore <title>'.

Archive is refused while any enabled task targets the session. The error names
every blocking task; disable or retarget them, then archive again. Agent Factory
never silently restores the target or disables its automation as a side effect.
While a session is archiving or archived, task writes that would newly enable it
as a target are likewise refused until the session is restored.

With --self, archive the current session (resolved via whoami) instead of a
named one — use it from inside a session when your work is done. --self and a
<title> argument are mutually exclusive.

Not available for remote or in-place (--here) sessions: archive relocates the
worktree, which those don't own. The relocated worktree path is printed on
success.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		title := ""
		if len(args) == 1 {
			title = args[0]
		}

		// --self resolves the caller's own session the way whoami does; it is
		// mutually exclusive with a positional <title>. The remote/in-place
		// guard needs no special handling here: --self routes through the same
		// daemon RPC as the title path, so the daemon still rejects a
		// non-relocatable worktree.
		var repoID string
		var sessionID string
		if sessionsArchiveSelf {
			if title != "" {
				return jsonError(fmt.Errorf("cannot combine --self with a <title> argument; --self archives the current session"))
			}
			data, err := resolveSelfSession()
			if err != nil {
				return jsonError(fmt.Errorf("--self must be run from inside an af session: %w", err))
			}
			title = data.Title
			// The resolved row's STABLE ID is the identity to act on, so send it:
			// the daemon resolves by id first and reports the repo id from the
			// row's own storage key, which is the authoritative one. Deriving
			// identity from a path instead is unsound for a worktree-less row —
			// its Worktree is empty, so sessionRepoID falls back to the recorded
			// workspace Path, and that path is mutable. Remove the checkout and
			// --self can no longer find its own session; let another repository
			// reuse the path and a same-titled session THERE is archived instead.
			// Under #3358 the row is pinned under the bare repository's id while
			// no field of InstanceData carries it, so the path is not even a
			// lossy spelling of the right answer.
			sessionID = data.ID
			// Scope by the RESOLVED session's OWN repo, never cwd/--repo. An
			// agent that cd'd into another repo must still archive ITS OWN
			// session — scoping by cwd would archive a same-titled namesake in
			// the wrong repo, or fail "instance not found" while leaving the
			// caller's real session alive. Mirror Storage's root→repoID
			// derivation (#667), shared with whoami via sessionRepoID so the
			// two cannot drift.
			// A worktree-less session (remote backend) leaves repoID empty so
			// the resolved title is matched all-repo and the daemon's remote
			// guard still fires with its own clear message.
			// This stays the fallback for a pre-#1195 row that has no id at all;
			// when the id is present the daemon ignores it.
			repoID = sessionRepoID(data)
		} else {
			if title == "" {
				return jsonError(fmt.Errorf("a session <title> is required (or pass --self to archive the current session)"))
			}
			// Honor --repo scoping (#761 class), mirroring kill: an empty repoID
			// preserves the all-repo search; a non-empty one confines the archive
			// to that repo so a same-titled session in another repo is never
			// touched.
			var err error
			repoID, err = resolveRepoID()
			if err != nil {
				return jsonError(err)
			}
		}

		archivedPath, err := archiveSessionViaDaemon(daemon.ArchiveSessionRequest{ID: sessionID, Title: title, RepoID: repoID})
		warning := ""
		if err != nil && apiclient.IsMutationCommitted(err) {
			warning = err.Error()
		} else if err != nil {
			return jsonError(err)
		}

		result := map[string]any{"ok": true, "title": title, "archived_path": archivedPath}
		if warning != "" {
			result["warning"] = warning
		}
		return jsonOut(result)
	},
}

var sessionsRestoreCmd = &cobra.Command{
	Use:   "restore <title>",
	Short: "Restore an archived, lost, or dead session",
	Long: `Restore a session that is currently archived, lost, or dead.

Archived sessions are moved back next to the repository, re-registered,
re-spawned, and marked running. Lost/dead sessions are recovered in place,
rebuilding a missing worktree when possible and resuming the recorded agent
conversation when required.

Fails if the session is not restorable, or if its origin repository is gone.
The restored worktree path is printed on success.

For a sandbox session (docker/ssh/hook) whose sandbox still ANSWERS but whose
agent is gone, restore first pushes the sandbox's work to origin, because
recovery re-clones from there and anything unpushed would be destroyed. If that
push fails, or if af cannot tell whether the sandbox is gone or merely
unreachable, the restore REFUSES and the session stays recoverable.

--force-reap replaces the sandbox anyway, without the push. It discards whatever
that sandbox has not pushed, so it is for a sandbox you know is expendable. It
applies to this one session and this one command; there is no global equivalent.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		forceReap, err := cmd.Flags().GetBool("force-reap")
		if err != nil {
			return jsonError(err)
		}

		worktreePath, err := restoreSessionViaDaemon(daemon.RestoreSessionRequest{Title: args[0], RepoID: repoID, ForceReap: forceReap})
		warning := ""
		if err != nil && apiclient.IsMutationCommitted(err) {
			warning = err.Error()
		} else if err != nil {
			return jsonError(err)
		}

		result := map[string]any{"ok": true, "title": args[0], "worktree_path": worktreePath}
		if warning != "" {
			result["warning"] = warning
		}
		return jsonOut(result)
	},
}
