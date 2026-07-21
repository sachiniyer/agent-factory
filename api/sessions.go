package api

import (
	"fmt"

	"os/exec"
	"strings"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"

	"github.com/spf13/cobra"
)

// SessionsCmd is the top-level command for session management.
var SessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage sessions",
}

var (
	createSessionViaDaemon  = daemon.CreateSession
	killSessionViaDaemon    = daemon.KillSession
	archiveSessionViaDaemon = daemon.ArchiveSession
	restoreSessionViaDaemon = daemon.RestoreSession
	sessionsArchiveSelf     bool
	sendPromptViaDaemon     = daemon.SendPrompt
	deliverPromptViaDaemon  = daemon.DeliverPrompt
	createTabViaDaemon      = daemon.CreateTab
	closeTabViaDaemon       = daemon.CloseTab
	renameTabViaDaemon      = daemon.RenameTab
	reorderTabViaDaemon     = daemon.ReorderTab
	previewSessionViaDaemon = func(req daemon.PreviewRequest) (string, bool, bool, error) {
		if !apiclient.IsRemoteTarget() {
			return daemon.PreviewSession(req)
		}
		client, err := apiclient.NewTargeted()
		if err != nil {
			return "", false, false, err
		}
		return client.Preview(req)
	}
	preflightLocalSession = preflight.LocalSessionPrereqs
	// snapshotViaDaemon is the non-spawning read path for list/get/whoami
	// (#1029 PR 2). It reflects the daemon's authoritative in-memory state when
	// a daemon is already running, and returns daemon.ErrDaemonUnavailable
	// (never spawning one) when it is not, so callers fall back to disk. Held in
	// a var so tests can inject a snapshot without a live daemon.
	//
	// #1592 Phase 2 PR2: this read now flows over the daemon's HTTP/JSON API
	// (apiclient) instead of net/rpc. The response is byte-identical — apiclient
	// decodes the same {data,error} envelope back into the same
	// session.InstanceData structs the RPC returned — so list/get/whoami output,
	// scoping, and disk-fallback behavior are unchanged; only the transport
	// moved. Every write/control path stays on net/rpc for now.
	snapshotViaDaemon = apiclient.SnapshotNoSpawn
)

// listSessions returns the session list for repoID (empty = all repos),
// preferring the daemon's live snapshot and falling back to disk when no daemon
// is reachable (#1029 PR 2). Both paths return the same shape sorted by
// (repoID, title), so scripts see a consistent order regardless of source.
func listSessions(repoID string) ([]session.InstanceData, error) {
	data, fallBack, err := snapshotRead(daemon.SnapshotRequest{RepoID: repoID})
	if err == nil {
		return data, nil
	}
	if !fallBack {
		return nil, err
	}
	return diskListSessions(repoID)
}

// getSessionByTitle returns the single session matching title across ALL repos,
// preferring the daemon's live snapshot and falling back to the disk scan when no
// daemon is reachable (#1029 PR 2). When a live snapshot is available the daemon
// is authoritative: a miss returns not-found without re-reading disk.
//
// Titles are unique per-repo, so this unscoped lookup resolves only when exactly
// one session matches; several matches return ErrAmbiguousTitle. Callers with a
// repo in hand should use getSessionByTitleInScope instead.
func getSessionByTitle(title string) (*session.InstanceData, error) {
	data, fallBack, err := snapshotRead(daemon.SnapshotRequest{})
	if err == nil {
		var matches []session.InstanceData
		for i := range data {
			if data[i].Title == title {
				matches = append(matches, data[i])
			}
		}
		// Group by repo path, not raw match count: the snapshot carries no
		// repoID, and only a title held by two distinct PROJECTS is ambiguous.
		paths := session.DedupeSorted(repoPathsOf(matches))
		if len(paths) > 1 {
			return nil, session.AmbiguousTitleError(title, repoPathsOf(matches))
		}
		if len(matches) > 0 {
			// One snapshot match is not proof of uniqueness: the snapshot mirrors
			// the daemon's m.instances, and refresh SKIPS rows it cannot restore
			// (worktree/tmux gone). A second repo holding the title on disk only
			// would be invisible here, so a bare `sessions get foo` would name the
			// wrong project. Union the local disk rows, mirroring the daemon's own
			// findSession guard.
			//
			// Only for a LOCAL target: a remote's sessions have nothing to do with
			// this machine's instances.json, and reading it would let a same-titled
			// local session make a remote lookup spuriously ambiguous. That leaves a
			// known gap — a REMOTE daemon in the same partial-restore state serves a
			// lone visible match and this read cannot see its disk to tell. Closing
			// it needs the guard on the daemon's side of the wire (a resolve-by-title
			// RPC that runs findSession, or a Snapshot that carries unrestorable
			// rows); the destructive paths already resolve through findSession.
			if !apiclient.IsRemoteTarget() {
				if extra, err := diskRepoPathsForTitle(title, paths); err == nil && len(extra) > 1 {
					return nil, session.AmbiguousTitleError(title, extra)
				}
			}
			return &matches[0], nil
		}
		// Mirror findInstanceByTitle's clean-miss error so output is unchanged.
		return nil, fmt.Errorf("session %q %w", title, errTitleNotFound)
	}
	// Remote target: no local disk fallback; surface the error (see snapshotRead).
	if !fallBack {
		return nil, err
	}
	got, _, err := findInstanceByTitle(title)
	return got, err
}

// whoamiSession returns the session whose TmuxName matches tmuxName, preferring
// the daemon's live snapshot and falling back to the disk scan when no daemon
// is reachable (#1029 PR 2).
func whoamiSession(tmuxName string) (*session.InstanceData, error) {
	data, fallBack, err := snapshotRead(daemon.SnapshotRequest{})
	if err == nil {
		for i := range data {
			if data[i].TmuxName == tmuxName {
				return &data[i], nil
			}
		}
		return nil, fmt.Errorf("no Agent Factory session found for tmux session %q", tmuxName)
	}
	// Remote target: no local disk fallback; surface the daemon error (e.g. a 401
	// from a bad token) instead of masking it with a same-machine disk scan (#1681).
	if !fallBack {
		return nil, err
	}
	return diskWhoami(tmuxName)
}

// currentTmuxName returns the tmux session name of the calling process. Held in
// a var so `whoami`/`archive --self` tests can resolve a session without a real
// tmux server.
var currentTmuxName = func() (string, error) {
	out, err := exec.Command("tmux", "display-message", "-p", "#{session_name}").Output()
	if err != nil {
		return "", fmt.Errorf("not running inside a tmux session: %w", err)
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("could not determine tmux session name")
	}
	return name, nil
}

// resolveSelfSession identifies the caller's own af session the same way
// `af sessions whoami` does: match the current tmux session name against the
// stored instances. Shared by the whoami command and `sessions archive --self`
// so the two cannot drift.
func resolveSelfSession() (*session.InstanceData, error) {
	tmuxName, err := currentTmuxName()
	if err != nil {
		return nil, err
	}
	return whoamiSession(tmuxName)
}

var sessionsListAllFlag bool

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sessions in the current project",
	Long: "List sessions in the current project.\n\n" +
		"Scope follows the shared project-context contract: --repo names a project, " +
		"otherwise the current directory's project is used, and --all spans every " +
		"project. Run from outside a git repository with no --repo, there is no " +
		"project context and every project's sessions are listed.",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if sessionsListAllFlag && repoFlag != "" {
			return jsonError(fmt.Errorf("--repo and --all are mutually exclusive: --repo names one project, --all spans every project"))
		}

		// Snapshot-based read: it follows --daemon-url to the remote, so it uses
		// the lookup resolver (which ignores the client's cwd against a remote).
		repoID := ""
		if !sessionsListAllFlag {
			var err error
			repoID, err = resolveRepoIDForLookup()
			if err != nil {
				return jsonError(err)
			}
		}

		// Read from the daemon's authoritative in-memory state when a daemon is
		// running, falling back to disk otherwise (#1029 PR 2). listSessions
		// never spawns a daemon, so `sessions list` in a script or CI keeps
		// working with none running.
		allData, err := listSessions(repoID)
		if err != nil {
			return jsonError(err)
		}

		if allData == nil {
			allData = []session.InstanceData{}
		}
		return jsonOut(allData)
	},
}

var sessionsGetCmd = &cobra.Command{
	Use:   "get <title>",
	Short: "Get a session by title",
	Long: `Titles are unique within a project, not across projects, so the same
name can exist in several repos. The title resolves inside the repo given by
--repo, or the current directory's repo when --repo is omitted.

With no repo context, a title held by exactly one session still resolves; one
held by sessions in several projects is ambiguous and reports an error naming
those projects instead of guessing between them.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// --repo is accepted on this command; it used to be parsed and then
		// silently dropped, so `get` always searched every repo and returned
		// whichever same-titled session the map walk hit first. This read is
		// served by the TARGETED daemon, so it uses the lookup resolver (which
		// ignores the client's cwd against a remote).
		repoID, err := resolveRepoIDForLookup()
		if err != nil {
			return jsonError(err)
		}
		data, err := getSessionByTitleInScope(repoID, args[0])
		if err != nil {
			return jsonError(err)
		}
		return jsonOut(data)
	},
}

var (
	createNameFlag    string
	createPromptFlag  string
	createProgramFlag string
	createHereFlag    bool
	createInPlaceFlag bool
	createBackendFlag string
)

var sessionsCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new session",
	Long: `Create a new session running an agent in its own git worktree.

With --here (alias --in-place) the session instead attaches to the repo's
existing working tree at its current branch: no worktree or branch is created,
the agent runs in the repo root, and killing the session never removes the
working tree or branch. Requires running inside a git repository (or --repo
pointing at one).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		inPlace := createHereFlag || createInPlaceFlag

		// resolveRepo already differentiates "--repo is required" (absent) from a
		// provided-but-invalid path and names the offending path (#892), so
		// surface its error verbatim instead of relabeling every failure as
		// "required".
		repo, err := resolveRepo()
		if err != nil {
			if inPlace {
				return jsonError(fmt.Errorf("--here requires a git repository to attach to: %w", err))
			}
			return jsonError(err)
		}

		if !git.IsGitRepo(repo.Root) {
			return jsonError(fmt.Errorf("path %s is not a git repository", repo.Root))
		}

		// Fail fast on the reserved root-agent title (#1106) before any
		// daemon round trip. The authoritative gate lives in the daemon's
		// reserveCreate; this mirrors its message for a snappier CLI error.
		if session.IsReservedTitle(createNameFlag) {
			return jsonError(fmt.Errorf("session title %q is reserved for the daemon-managed root agent; pick another name (to run a root agent on this repo, add it to root_agents in ~/.agent-factory/config.json)", createNameFlag))
		}

		// Best-effort per-repo pre-check to fail fast on duplicate titles
		// before we spend time creating a tmux session and git worktree we'd
		// just have to tear down. The authoritative race-safe check still
		// happens inside the daemon under the per-repo file lock.
		exists, err := repoHasInstanceTitle(repo.ID, createNameFlag)
		if err != nil {
			return jsonError(err)
		}
		if exists {
			return jsonError(fmt.Errorf("session with title %q already exists", createNameFlag))
		}

		cfg, err := config.ResolveConfig(repo.Root)
		if err != nil {
			return jsonError(err)
		}

		program := createProgramFlag
		if program == "" {
			program = cfg.DefaultProgram
		} else if err := config.ValidateProgramEnum("--program flag", "--program flag", program, ""); err != nil {
			return jsonError(err)
		}
		if err := preflightLocalSession(&cfg.Config, program); err != nil {
			return jsonError(err)
		}

		// Validate --backend up front so a typo fails on the client with a clear
		// message rather than after a daemon round trip (#1592 Phase 4 PR3). An
		// empty flag defers to the repo's `backend` config key.
		if _, err := session.ParseBackendKind(createBackendFlag); err != nil {
			return jsonError(err)
		}
		if inPlace && createBackendFlag != "" && createBackendFlag != config.BackendLocal {
			return jsonError(fmt.Errorf("--here runs in the local working tree and is incompatible with --backend %s", createBackendFlag))
		}

		data, err := createSessionViaDaemon(daemon.CreateSessionRequest{
			Title:    createNameFlag,
			RepoPath: repo.Root,
			Program:  program,
			Prompt:   createPromptFlag,
			AutoYes:  cfg.AutoYes,
			InPlace:  inPlace,
			Backend:  createBackendFlag,
		})
		if err != nil {
			return jsonError(err)
		}

		return jsonOut(data)
	},
}

var (
	previewTabFlag     int
	previewTabNameFlag string
	previewTabIDFlag   string
	previewFullFlag    bool
)

// previewTabMissErr is the message for a tab-level miss — an --tab-id that no
// longer resolves, or a --tab that is not a slot.
//
// Both local and remote previews are daemon-resolved and report the miss through
// TabGone, so a user cannot tell which machine resolved the selector. It names
// the FLAG the user passed, which neither the daemon's session-oriented error nor
// a bare "gone" can do.
//
// A --tab-name miss is deliberately NOT here: it is answered with the session's
// roster, which only the side holding the tab list can produce.
func previewTabMissErr() error {
	if previewTabIDFlag != "" {
		return fmt.Errorf("--tab-id %q matches no tab in this session; it may have been closed",
			previewTabIDFlag)
	}
	return fmt.Errorf("--tab %d is not a slot in this session", previewTabFlag)
}

var sessionsPreviewCmd = &cobra.Command{
	Use:   "preview <title>",
	Short: "Preview a session's terminal content",
	Long: `Titles are unique within a project, not across projects, so the same
name can exist in several repos. The title resolves inside the repo given by
--repo, or the current directory's repo when --repo is omitted.

With no repo context, a title held by exactly one session still resolves; one
held by sessions in several projects is ambiguous and reports an error naming
those projects instead of guessing between them.

By default this captures the session's AGENT tab (slot 0), visible screen only.
Address another tab with --tab-name (the tab name "af sessions tab-create"
printed, as reported by "af sessions get" — not the TUI's "Agent"/"Terminal"
label), --tab-id (the stable id, for scripts that must not follow a reused
name), or --tab (the 0-based slot). They resolve in that precedence — id, then
name, then slot — the same order every tab verb uses. An id or name that does
not resolve is an error, never a silent fall back to a slot: that would capture
whatever tab had shifted into it.

--full returns the entire scrollback instead of the visible screen.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// --repo is accepted on this command; it used to be parsed and then
		// silently dropped. That was worse here than on `get`: resolving the
		// wrong repo's session does not just read it, it restores/starts it.
		repoID, err := resolveRepoIDForLookup()
		if err != nil {
			return jsonError(err)
		}

		// Capture through the daemon on both local and remote targets. Rebuilding a
		// local Instance here used to restore the session in this short-lived CLI
		// process, which also ran the configured shell-command probe before the one
		// capture-pane the command actually needed. The daemon already owns the
		// canonical live Instance and is the sole capture path, so selectors travel
		// there unresolved and are applied atomically against its current tab roster.
		content, gone, tabGone, err := previewSessionViaDaemon(daemon.PreviewRequest{
			Title:   args[0],
			RepoID:  repoID,
			Tab:     previewTabFlag,
			TabName: previewTabNameFlag,
			TabID:   previewTabIDFlag,
			Full:    previewFullFlag,
		})
		if err != nil {
			return jsonError(err)
		}
		// A tab-level miss is NOT a dead session. Reporting one as the other
		// tells the user their running session ended because they mistyped a
		// selector. The daemon carries the distinction so this path never guesses.
		if tabGone {
			return jsonError(previewTabMissErr())
		}
		if gone {
			return jsonError(fmt.Errorf("session %q is no longer running", args[0]))
		}
		return jsonOut(map[string]string{
			"title":   args[0],
			"content": content,
		})
	},
}

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
		if sessionsArchiveSelf {
			if title != "" {
				return jsonError(fmt.Errorf("cannot combine --self with a <title> argument; --self archives the current session"))
			}
			data, err := resolveSelfSession()
			if err != nil {
				return jsonError(fmt.Errorf("--self must be run from inside an af session: %w", err))
			}
			title = data.Title
			// Scope by the RESOLVED session's OWN repo, never cwd/--repo. An
			// agent that cd'd into another repo must still archive ITS OWN
			// session — scoping by cwd would archive a same-titled namesake in
			// the wrong repo, or fail "instance not found" while leaving the
			// caller's real session alive. Mirror Storage's root→repoID
			// derivation (#667), shared with whoami via sessionRepoRoot so the
			// two cannot drift.
			// A worktree-less session (remote backend) leaves repoID empty so
			// the resolved title is matched all-repo and the daemon's remote
			// guard still fires with its own clear message.
			root := sessionRepoRoot(data)
			if root != "" {
				repoID = config.RepoIDFromRoot(root)
			}
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

		archivedPath, err := archiveSessionViaDaemon(daemon.ArchiveSessionRequest{Title: title, RepoID: repoID})
		if err != nil {
			return jsonError(err)
		}

		return jsonOut(map[string]any{"ok": true, "title": title, "archived_path": archivedPath})
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
The restored worktree path is printed on success.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		worktreePath, err := restoreSessionViaDaemon(daemon.RestoreSessionRequest{Title: args[0], RepoID: repoID})
		if err != nil {
			return jsonError(err)
		}

		return jsonOut(map[string]any{"ok": true, "title": args[0], "worktree_path": worktreePath})
	},
}

// resolveAttachTarget resolves the {title, repoID} pair `sessions attach` hands
// to the daemon's WS PTY stream.
//
// --repo scoping is honored (#891, same class as #761/#776): an empty repoID
// preserves the all-repo search, a non-empty one confines the attach to that
// repo so `attach <title> --repo <other>` can never connect the terminal to a
// same-titled session in a different repo.
//
// The remote branch is #1974. Attach runs over the apiclient transport, so it
// follows --daemon-url to another machine — but it was resolving the session on
// THIS machine's disk first (resolveRepoID + findLiveInstanceByTitleInScope),
// which fails "session not found" before any remote call for a session that
// only exists on the daemon. Both halves were wrong against a remote: the repo
// ID hashes the CLIENT's cwd, naming a repo the daemon has never heard of, and
// the instances.json read has no bearing on what the remote holds. So a remote
// target hands the bare title to the daemon, which resolves it on its own side
// (by id from memory, then by title from its own disk) — mirroring the preview
// command's remote branch above, the other apiclient-transport read.
//
// The LOCAL path is deliberately unchanged: resolveRepoIDForLookup is identical
// to resolveRepoID when no remote target is set, and the disk lookup still
// restores the instance, which is what gives the local daemon a session to
// attach to.
func resolveAttachTarget(title string) (string, string, error) {
	repoID, err := resolveRepoIDForLookup()
	if err != nil {
		return "", "", err
	}
	if apiclient.IsRemoteTarget() {
		return title, repoID, nil
	}
	instance, _, err := findLiveInstanceByTitleInScope(repoID, title)
	if err != nil {
		return "", "", err
	}
	return instance.Title, repoID, nil
}

var sessionsAttachCmd = &cobra.Command{
	Use:   "attach <title>",
	Short: "Attach to a session's terminal",
	Long:  "Attach to a running session's tmux terminal. Detach with the configured detach key (default: Ctrl-w).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		title, repoID, err := resolveAttachTarget(args[0])
		if err != nil {
			return jsonError(err)
		}

		// EVERY session — local or remote — attaches CLIENT-side over the daemon's
		// WS PTY stream (#1592 Phase 2 PR7), the same path the TUI uses: the daemon
		// resolves the byte source via instance.AgentServer() (a local broker, or a
		// remoteAgentServer proxy for docker/ssh/hook). A prior
		// Capabilities().Workspace == WorkspaceRemote branch called instance.Attach()
		// here, aimed at a backend attach that had already become a routing-invariant
		// error, so it broke remote attach outright (#1837). That surface is now
		// deleted (#1852) — this stream dial is the only attach there is.
		//
		// EnsureDaemon spawns the LOCAL daemon that owns a local session's
		// clientless broker; a remote target's daemon is already running on the
		// other machine, so skip the local spawn and dial it directly (#1592
		// Phase 3 PR4).
		if !apiclient.IsRemoteTarget() {
			if err := daemon.EnsureDaemon(); err != nil {
				return jsonError(fmt.Errorf("failed to reach daemon for attach: %w", err))
			}
		}
		client, err := apiclient.NewTargeted()
		if err != nil {
			return jsonError(err)
		}
		// The CLI attaches the agent tab (index 0), which is structurally always
		// first and never shifts, so a positional address is unambiguous — no stable
		// tab id needed here (#1738).
		detached, err := client.AttachStream(cmd.Context(), title, repoID, "", 0)
		if err != nil {
			return jsonError(fmt.Errorf("failed to attach: %w", err))
		}
		<-detached
		return nil
	},
}

var sessionsWhoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Identify the current Agent Factory session",
	Long: "Returns the session info for the current tmux session by matching the tmux session name against stored sessions.\n\n" +
		"Identity is not scoped: you are the session you are, in whatever project it " +
		"belongs to. --repo therefore acts as an assertion — it checks that the " +
		"resolved session really is in that project, and errors if it is not.",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// Match the current tmux session against the daemon's authoritative
		// in-memory state when a daemon is running, falling back to a disk scan
		// otherwise (#1029 PR 2). Never spawns a daemon.
		data, err := resolveSelfSession()
		if err != nil {
			return jsonError(err)
		}

		// whoami inherits the persistent --repo but used to parse and drop it —
		// the same silent mis-resolution #1814 fixed for get/preview. It cannot
		// SCOPE the lookup (the caller's tmux name already names exactly one
		// session, repo hash included), so an explicit --repo is checked instead
		// of ignored: a caller who names the wrong project learns that rather
		// than receiving another project's answer with no signal (#1893).
		if repoFlag != "" {
			// Resolve and VALIDATE the flag first, before asking whether there
			// is anything to compare it against. Gating the whole block on the
			// session having a root meant a row with neither Worktree.RepoPath
			// nor Path (some remote-backed rows) skipped this entirely, so
			// `whoami --repo /not-a-repo` succeeded and printed session data —
			// an explicitly malformed flag silently ignored (#1893 review). What
			// the flag NAMES is checkable on its own; whether it MATCHES is the
			// separate question below.
			repo, err := repoFromFlag()
			if err != nil {
				return jsonError(err)
			}
			// A session that records no repo root at all has nothing to compare
			// against: asserting on a value we do not have would fail a caller
			// who IS in the named project, so an unknown project is never an
			// error — only a known-mismatched one.
			//
			// Resolve the session's root through git rather than hashing it
			// raw: a stored root that was never git-resolved would otherwise
			// hash differently from the canonical --repo naming the same
			// project, rejecting a caller who is exactly where they claim.
			if root := sessionRepoRoot(data); root != "" && newProjectIDCache().idFor(root) != repo.ID {
				return jsonError(fmt.Errorf("this session belongs to project %s, not --repo %s", root, repo.Root))
			}
		}
		return jsonOut(*data)
	},
}
