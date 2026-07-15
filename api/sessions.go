package api

import (
	"errors"
	"fmt"
	"sort"

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
	preflightLocalSession   = preflight.LocalSessionPrereqs
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

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sessions",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// Snapshot-based read: it follows --daemon-url to the remote, so it uses
		// the lookup resolver (which ignores the client's cwd against a remote).
		repoID, err := resolveRepoIDForLookup()
		if err != nil {
			return jsonError(err)
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
	sendPromptCreateFlag      bool
	sendPromptProgramFlag     string
	sendPromptAllFlag         bool
	sendPromptAllReposFlag    bool
	sendPromptIncludeRootFlag bool
)

var sessionsSendPromptCmd = &cobra.Command{
	Use:   "send-prompt <title> <prompt>",
	Short: "Send a prompt to a session (or broadcast to all with --all)",
	Long: `Send a prompt to an existing session. The session must already exist unless --create is used.

If the session does not exist, use --create to automatically create it first,
or use 'af sessions create --name <title> --prompt <prompt>' instead.

With --all, broadcast a single prompt to every live session in scope:

    af sessions send-prompt --all "<prompt>"

Broadcast scope defaults to the current repo (honoring --repo). Pass --all-repos
to broadcast across every repo. The reserved root session is excluded unless
--include-root is given. Delivery is best-effort per session: unreachable (Lost,
Dead) and Archived sessions are skipped and reported, and one failure never
aborts the rest. The command prints a JSON summary (delivered / failed / skipped)
and exits 0 even when some sessions fail, so scripts can inspect per-session
results.`,
	// Validate flag combinations before arity (cobra runs Args before RunE):
	// a broadcast-implying flag without --all must surface its actionable
	// message here, not cobra's generic "accepts 2 arg(s)" (#658/#734: public
	// CLI = actionable errors). Arity is then mode-aware — with --all the single
	// positional is the prompt; otherwise it's <title> <prompt>. Flags are
	// parsed before Args runs, so the mode flags are already set here.
	Args: func(cmd *cobra.Command, args []string) error {
		if err := validateSendPromptFlags(); err != nil {
			return jsonError(err)
		}
		if sendPromptAllFlag {
			if len(args) != 1 {
				return jsonError(fmt.Errorf("--all broadcast takes exactly one argument (the prompt to broadcast); got %d", len(args)))
			}
			return nil
		}
		return cobra.ExactArgs(2)(cmd, args)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// Re-check here too: unit tests drive RunE directly (bypassing Args),
		// and it is cheap defense-in-depth against a future caller that skips
		// arg validation. In the real CLI Args already caught these.
		if err := validateSendPromptFlags(); err != nil {
			return jsonError(err)
		}
		if sendPromptAllFlag {
			return runBroadcast(args[0])
		}

		title := args[0]
		prompt := args[1]

		// Honor --repo scoping (#776, follow-up to #761/#775). An empty repoID
		// preserves the prior all-repo search; a non-empty one confines both
		// the existence pre-check and the delivery to that repo so a
		// same-titled session in a different repo never receives the prompt.
		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		// --create routes through the daemon's serialized create-or-send path
		// so a session that pops into existence concurrently (another
		// --create, or a task delivering into the same target_session) is
		// delivered into rather than racing creation and dropping a prompt
		// (#865). The daemon decides create-vs-send under its per-target lock,
		// so no existence pre-check is needed here.
		if sendPromptCreateFlag {
			// resolveRepo distinguishes absent --repo ("--repo is required")
			// from a provided-but-invalid path and names it (#892). --create is
			// the only send-prompt mode that needs a resolvable repo, so surface
			// that error directly rather than relabeling an invalid path as
			// "required".
			repo, repoErr := resolveRepo()
			if repoErr != nil {
				return jsonError(repoErr)
			}

			if !git.IsGitRepo(repo.Root) {
				return jsonError(fmt.Errorf("path %s is not a git repository", repo.Root))
			}

			cfg, err := config.ResolveConfig(repo.Root)
			if err != nil {
				return jsonError(err)
			}

			program := sendPromptProgramFlag
			if program == "" {
				program = cfg.DefaultProgram
			} else if err := config.ValidateProgramEnum("--program flag", "--program flag", program, ""); err != nil {
				return jsonError(err)
			}
			if err := preflightLocalSession(&cfg.Config, program); err != nil {
				return jsonError(err)
			}

			if _, err := deliverPromptViaDaemon(daemon.DeliverPromptRequest{
				Title:    title,
				RepoPath: repo.Root,
				Program:  program,
				Prompt:   prompt,
				AutoYes:  cfg.AutoYes,
			}); err != nil {
				return jsonError(err)
			}
			return jsonOut(map[string]bool{"ok": true})
		}

		exists, err := instanceTitleExistsInScope(repoID, title)
		if err != nil {
			return jsonError(err)
		}
		if !exists {
			return jsonError(fmt.Errorf("session %q not found. Use --create to auto-create the session, or run: af sessions create --name %q --prompt <prompt>", title, title))
		}

		if err := sendPromptViaDaemon(daemon.SendPromptRequest{Title: title, RepoID: repoID, Prompt: prompt}); err != nil {
			return jsonError(err)
		}
		return jsonOut(map[string]bool{"ok": true})
	},
}

// broadcastResult is the JSON summary `send-prompt --all` prints: aggregate
// counts plus a per-session breakdown so scripts can tell exactly which
// sessions received the prompt and why any did not.
type broadcastResult struct {
	Prompt    string            `json:"prompt"`
	Scope     string            `json:"scope"`
	Delivered int               `json:"delivered"`
	Failed    int               `json:"failed"`
	Skipped   int               `json:"skipped"`
	Results   []broadcastTarget `json:"results"`
}

// broadcastTarget is one session's outcome in a broadcast. Status is one of
// "delivered", "failed", or "skipped"; Error carries the daemon's reason on a
// failure and Reason explains an intentional skip (root excluded, session lost).
type broadcastTarget struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// validateSendPromptFlags rejects nonsensical send-prompt flag combinations
// with an actionable message (public CLI standard, #658/#734). It runs from both
// Args — so it fires before cobra's arity check, which would otherwise mask a
// broadcast flag used without --all behind a generic "accepts 2 arg(s)" error —
// and RunE, so unit tests that drive RunE directly still get the same guard.
func validateSendPromptFlags() error {
	if !sendPromptAllFlag {
		// The broadcast-only flags are meaningless without --all. Name whichever
		// were passed and point the user at the flag that unlocks them.
		var needsAll []string
		if sendPromptAllReposFlag {
			needsAll = append(needsAll, "--all-repos")
		}
		if sendPromptIncludeRootFlag {
			needsAll = append(needsAll, "--include-root")
		}
		if len(needsAll) > 0 {
			return fmt.Errorf("%s can only be used with --all (broadcast mode); add --all to broadcast the prompt to every session in scope", strings.Join(needsAll, " and "))
		}
		return nil
	}
	if sendPromptCreateFlag {
		return errors.New("--all cannot be combined with --create: broadcast only delivers to existing sessions")
	}
	if sendPromptAllReposFlag && repoFlag != "" {
		return errors.New("--all-repos and --repo are mutually exclusive: --all-repos already spans every repo")
	}
	return nil
}

// runBroadcast implements `af sessions send-prompt --all`: deliver one prompt to
// every live session in scope via the same daemon SendPrompt RPC the single-
// target path uses. Scope defaults to the current repo (honoring --repo) so a
// broadcast can never blast another repo's sessions (#761 data-loss class);
// --all-repos opts into spanning every repo. The reserved root session is
// excluded unless --include-root. Delivery is best-effort: a Lost/unreachable
// target is reported and skipped, a per-session send error is recorded, and
// neither aborts the rest. The command exits 0 with a JSON summary regardless of
// individual failures so callers inspect the per-session results.
func runBroadcast(prompt string) error {
	var (
		targets    []scopedInstance
		scopeLabel string
	)
	if sendPromptAllReposFlag {
		all, corrupted, err := allScopedInstances()
		if err != nil {
			return jsonError(err)
		}
		// Fail loudly on a corrupted repo rather than silently broadcasting to
		// a truncated set — the same loud-fail contract as `sessions list`
		// (#730). A hidden session that never receives the prompt is worse than
		// an error the user can act on.
		if len(corrupted) > 0 {
			return jsonError(corruptedReposError(corrupted))
		}
		targets = all
		scopeLabel = "all-repos"
	} else {
		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}
		if repoID == "" {
			// Refuse to guess the scope: silently broadcasting to every repo
			// here is exactly the #761 wrong-repo hazard the --repo scoping
			// exists to prevent.
			return jsonError(errors.New("broadcast needs a target repo: run inside a git repository, pass --repo <path>, or use --all-repos to broadcast to every repo"))
		}
		scoped, err := scopedInstancesForRepo(repoID)
		if err != nil {
			return jsonError(err)
		}
		targets = scoped
		scopeLabel = "repo:" + repoID
	}

	// Deterministic order (repo, then title) so output is stable across runs
	// and the all-repos map iteration order does not leak into the summary.
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].RepoID != targets[j].RepoID {
			return targets[i].RepoID < targets[j].RepoID
		}
		return targets[i].Title < targets[j].Title
	})

	result := broadcastResult{Prompt: prompt, Scope: scopeLabel, Results: []broadcastTarget{}}
	for _, t := range targets {
		// The reserved root session belongs to the maintainer agent (#1106):
		// don't broadcast into it unless explicitly asked.
		if session.IsReservedTitle(t.Title) && !sendPromptIncludeRootFlag {
			result.Skipped++
			result.Results = append(result.Results, broadcastTarget{
				Title:  t.Title,
				RepoID: t.RepoID,
				Status: "skipped",
				Reason: "reserved root session excluded; pass --include-root to broadcast to it",
			})
			continue
		}
		// Lost/Dead sessions have no live backing session to deliver into
		// (#1108). Report them as skipped-unreachable instead of attempting a
		// send that would only fail — the broadcast tolerates them cleanly.
		if t.Status == session.Lost || t.Status == session.Dead {
			result.Skipped++
			result.Results = append(result.Results, broadcastTarget{
				Title:  t.Title,
				RepoID: t.RepoID,
				Status: "skipped",
				Reason: "session is lost/unreachable; recover it before broadcasting",
			})
			continue
		}
		// Archived sessions are deliberately inert (#1028): there is no running
		// backend to receive a prompt until the user restores them.
		if t.Status == session.Archived {
			result.Skipped++
			result.Results = append(result.Results, broadcastTarget{
				Title:  t.Title,
				RepoID: t.RepoID,
				Status: "skipped",
				Reason: "session is archived; restore it before broadcasting",
			})
			continue
		}
		if err := sendPromptViaDaemon(daemon.SendPromptRequest{Title: t.Title, RepoID: t.RepoID, Prompt: prompt}); err != nil {
			result.Failed++
			result.Results = append(result.Results, broadcastTarget{
				Title:  t.Title,
				RepoID: t.RepoID,
				Status: "failed",
				Error:  err.Error(),
			})
			continue
		}
		result.Delivered++
		result.Results = append(result.Results, broadcastTarget{
			Title:  t.Title,
			RepoID: t.RepoID,
			Status: "delivered",
		})
	}
	return jsonOut(result)
}

var sessionsPreviewCmd = &cobra.Command{
	Use:   "preview <title>",
	Short: "Preview a session's terminal content",
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
		// silently dropped. That was worse here than on `get`: resolving the
		// wrong repo's session does not just read it, it restores/starts it.
		repoID, err := resolveRepoIDForLookup()
		if err != nil {
			return jsonError(err)
		}

		// A remote target must be previewed BY the remote daemon. The local path
		// below reads this machine's instances.json and restores the session to
		// capture it — against --daemon-url that would preview (and start) a
		// same-titled LOCAL session, or report not-found even though the remote
		// holds the session. The daemon resolves {title, repoID} on its own side,
		// where the sessions actually live.
		if apiclient.IsRemoteTarget() {
			client, cerr := apiclient.NewTargeted()
			if cerr != nil {
				return jsonError(cerr)
			}
			content, gone, perr := client.Preview(daemon.PreviewRequest{Title: args[0], RepoID: repoID})
			if perr != nil {
				return jsonError(perr)
			}
			if gone {
				return jsonError(fmt.Errorf("session %q is no longer running", args[0]))
			}
			return jsonOut(map[string]string{
				"title":   args[0],
				"content": content,
			})
		}

		instance, _, err := findLiveInstanceByTitleInScope(repoID, args[0])
		if err != nil {
			return jsonError(err)
		}

		content, err := instance.AgentServer().Preview(0, false)
		if err != nil {
			return jsonError(fmt.Errorf("failed to get preview: %w", err))
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
			// derivation (#667): fall back to Path when no worktree RepoPath.
			// A worktree-less session (remote backend) leaves repoID empty so
			// the resolved title is matched all-repo and the daemon's remote
			// guard still fires with its own clear message.
			root := data.Worktree.RepoPath
			if root == "" {
				root = data.Path
			}
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

var sessionsAttachCmd = &cobra.Command{
	Use:   "attach <title>",
	Short: "Attach to a session's terminal",
	Long:  "Attach to a running session's tmux terminal. Detach with the configured detach key (default: Ctrl-w).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// Honor --repo scoping (#891, same class as #761/#776). An empty repoID
		// preserves the prior all-repo search; a non-empty one confines the
		// attach to that repo so `attach <title> --repo <other>` can never
		// connect the terminal to a same-titled session in a different repo.
		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		instance, _, err := findLiveInstanceByTitleInScope(repoID, args[0])
		if err != nil {
			return jsonError(err)
		}

		// A remote session attaches its hook attach_cmd PTY in-process; a local
		// session attaches CLIENT-side over the daemon's WS PTY stream (#1592
		// Phase 2 PR7), the same path the TUI uses. Ensure a daemon is up first —
		// it owns the local session's clientless broker — then dial it.
		if instance.Capabilities().Workspace == session.WorkspaceRemote {
			detached, err := instance.Attach()
			if err != nil {
				return jsonError(fmt.Errorf("failed to attach: %w", err))
			}
			<-detached
			return nil
		}

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
		detached, err := client.AttachStream(cmd.Context(), instance.Title, repoID, "", 0)
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
	Long:  "Returns the session info for the current tmux session by matching the tmux session name against stored sessions.",
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
		return jsonOut(*data)
	},
}
