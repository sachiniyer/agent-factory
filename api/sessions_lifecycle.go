package api

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// The three session-lifecycle verbs — kill, archive, restore — moved out of
// api/sessions.go, which had grown to the file-length limit (#1145) as the
// package's residual grab-bag. This follows the split the rest of the package
// already uses (sessions_backends.go, sessions_handoff.go, sessions_watch.go):
// one file per command cluster. Their daemon seams stay in api/sessions.go with
// the other session verbs, so tests substitute them from one place.

var sessionsKillCmd = &cobra.Command{
	Use:   "kill <title>",
	Short: "Permanently delete a session and af-owned resources",
	Long: `Permanently delete the session record and stop its terminals. Remove only
worktrees and branches owned by af; user-owned resources stay.

For normal "done with this session" cleanup, prefer:
  af sessions archive <title>

Deletion is permanent. Uncommitted or unmerged work in af-owned resources may
be lost. Archive instead to keep the session restorable.
--force is accepted but has no effect (kept for backward compatibility).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// Honor --repo scoping (#761). An empty repoID preserves the prior
		// all-repo search; a non-empty one confines the kill to that repo so a
		// same-titled session in a different repo is never destroyed by
		// mistake. Mirrors sessionsListCmd's resolveRepoID() usage.
		repoID, err := resolveRepoIDForLookup()
		if err != nil {
			return jsonError(err)
		}

		// --force is accepted for backward compatibility but
		// is a no-op: it is intentionally NOT forwarded to the daemon, whose
		// KillSessionRequest no longer carries a force field (#1579).
		//
		// A kill whose durable tombstone committed but whose post-commit
		// teardown/storage follow-up failed is reported as success-with-
		// warning, exactly as archive/restore do for the same committed-
		// mutation outcome. The tombstone is durable and the row is retained
		// for the asynchronous finishUserKill/ghost-worker reap, so a non-zero
		// exit would tell automation a committed kill "failed" and invite a
		// retry that races the in-flight guard or re-tombstones an already-
		// UserKilled row (#3252).
		// Ignore SIGHUP across the daemon call: a kill of the session this CLI
		// is running inside closes the pane's pty under it, and the kernel
		// hangup would otherwise kill the caller before the reply (#5182).
		defer ignoreTeardownHangup()()
		kept, err := killSessionViaDaemon(daemon.KillSessionRequest{Title: args[0], RepoID: repoID})
		warning := ""
		if err != nil && apiclient.IsMutationCommitted(err) {
			warning = err.Error()
		} else if err != nil {
			return jsonError(err)
		}

		result := map[string]any{"ok": true}
		if warning != "" {
			result["warning"] = warning
		}
		// A branch af does not own is left in the repo with its commits; say so
		// in the result rather than leaving a stray ref unexplained (#6233).
		if kept.Any() {
			result["kept_branch"] = kept.Name
			result["note"] = kept.Note()
		}
		return jsonOut(result)
	},
}

var sessionsArchiveCmd = &cobra.Command{
	Use:   "archive [title]",
	Short: "Finish with a session by archiving it for later restore",
	Long: `Archive keeps a session restorable. Locally, stop its terminals
and move its owned git worktree to the global archive directory
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

An ssh or sandbox session publishes nothing by default: the agent stops and the
workspace stays on its host, owned by the archived row, and the output names where
(archived_workspace). Pass --push to push the branch to origin first; docker and
hook sessions always push. Restoring a session archived without a push
reattaches that workspace — the tree, index and ignored files come back exactly as
archived and the agent resumes its conversation; nothing is cloned, pushed or
re-hooked. If the host proves the workspace gone, restore re-provisions onto the
recorded branch or fails. In-place (--here) sessions cannot be archived because af does not own
their worktree. Local archives print the relocated worktree path on success.`,
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
			// Against a remote daemon the caller is whoever the CREDENTIAL says it
			// is: a sandbox credential is bound to its own session, which the
			// daemon reports (and whose route admits this verb). This machine's
			// tmux name would be a guess about someone else's registry.
			var data *session.InstanceData
			var err error
			if apiclient.IsRemoteTarget() {
				data, err = resolveCredentialSelf()
			} else {
				data, err = resolveSelfSession()
			}
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
			repoID, err = resolveRepoIDForLookup()
			if err != nil {
				return jsonError(err)
			}
		}

		// --self's reason for existing: same SIGHUP shield as kill (#5182).
		defer ignoreTeardownHangup()()
		archivedPath, err := archiveSessionViaDaemon(daemon.ArchiveSessionRequest{ID: sessionID, Title: title, RepoID: repoID, Push: sessionsArchivePush})
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
		// Where an off-box session's work lives: the preserved directory on its
		// host, or the pushed branch (#5571 W8.5). Best-effort, from the archived
		// row; a local archive has none and the key is absent.
		if loc := archivedWorkspaceOf(repoID, sessionID, title); loc != nil {
			result["archived_workspace"] = loc
		}
		return jsonOut(result)
	},
}

// archivedWorkspaceOf reads back where an archived off-box session's work lives.
// Best-effort: any read failure just omits the additive field.
func archivedWorkspaceOf(repoID, sessionID, title string) *session.ArchivedWorkspaceData {
	rows, _, err := snapshotViaDaemon(daemon.SnapshotRequest{RepoID: repoID})
	if err != nil {
		return nil
	}
	for _, d := range rows {
		if d.ArchivedWorkspace == nil {
			continue
		}
		if (sessionID != "" && d.ID == sessionID) || (sessionID == "" && d.Title == title) {
			return d.ArchivedWorkspace
		}
	}
	return nil
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
agent is gone, restore relaunches the agent in place: the sandbox's
agent-server stops the agent and starts it again in its existing workspace,
so the branch, the tree and any uncommitted work are untouched, and nothing
is pushed, reaped, re-cloned or re-provisioned. A sandbox whose agent-server
does not advertise the relaunch feature on /v1/health (an older build, before
the relaunch route existed) falls back to replacing the sandbox: that pushes
its work to origin first, because recovery re-clones from there and anything
unpushed would be destroyed. If that push fails, or if af cannot tell whether
the sandbox is gone or merely unreachable, the restore REFUSES and the session
stays recoverable.

--force-reap replaces the sandbox anyway, skipping the relaunch and the push.
It discards whatever that sandbox has not pushed, so it is for a sandbox you
know is expendable. It applies to this one session and this one command; there
is no global equivalent.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repoID, err := resolveRepoIDForLookup()
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
