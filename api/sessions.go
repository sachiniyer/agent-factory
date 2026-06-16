package api

import (
	"encoding/json"
	"fmt"

	"os/exec"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/spf13/cobra"
)

// SessionsCmd is the top-level command for session management.
var SessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage sessions",
}

var (
	createSessionViaDaemon = daemon.CreateSession
	killSessionViaDaemon   = daemon.KillSession
	sendPromptViaDaemon    = daemon.SendPrompt
	deliverPromptViaDaemon = daemon.DeliverPrompt
)

// ghostKillTmuxByName issues a tmux kill-session for a persisted sanitized
// name. Package-level so tests can stub it without invoking real tmux. The
// af_ prefix check refuses to act on names the daemon would never write, so a
// corrupted store can't make us kill an unrelated tmux session.
var ghostKillTmuxByName = func(sanitizedName string) error {
	if !strings.HasPrefix(sanitizedName, tmux.TmuxPrefix) {
		return fmt.Errorf("refusing to kill tmux session without %q prefix: %q", tmux.TmuxPrefix, sanitizedName)
	}
	return tmux.NewTmuxSessionFromSanitizedName(sanitizedName, "").CloseAndWaitForPaneExit()
}

// ghostCleanupWorktree performs best-effort worktree teardown for a ghost
// session whose live restore failed. Package-level so tests can stub it.
var ghostCleanupWorktree = func(data *session.InstanceData, title string) {
	if data.Worktree.RepoPath == "" || data.Worktree.WorktreePath == "" || data.Worktree.ExternalWorktree {
		return
	}
	branchCreatedByUs := true
	if data.Worktree.BranchCreatedByUs != nil {
		branchCreatedByUs = *data.Worktree.BranchCreatedByUs
	}
	gw, gwErr := git.NewGitWorktreeFromStorage(
		data.Worktree.RepoPath,
		data.Worktree.WorktreePath,
		data.Worktree.SessionName,
		data.Worktree.BranchName,
		data.Worktree.BaseCommitSHA,
		data.Worktree.ExternalWorktree,
		branchCreatedByUs,
	)
	if gwErr != nil {
		log.WarningLog.Printf("ghost session %q: failed to load worktree for cleanup: %v", title, gwErr)
		return
	}
	if cleanupErr := gw.Cleanup(); cleanupErr != nil {
		log.WarningLog.Printf("ghost session %q: worktree cleanup failed: %v", title, cleanupErr)
	}
}

// ghostCleanup runs best-effort teardown of a ghost session's external
// resources. Tmux teardown is independent of worktree state (#516): a ghost
// record can have an empty worktree path while a tmux session with the
// persisted name is still running, so the two branches share no condition.
// Tmux goes FIRST: a still-running agent writing into the worktree while git
// recursively deletes it leaks a half-deleted directory (#802).
func ghostCleanup(data *session.InstanceData, title string) {
	if data.TmuxName != "" {
		if killErr := ghostKillTmuxByName(data.TmuxName); killErr != nil {
			log.WarningLog.Printf("ghost session %q: tmux cleanup failed: %v", title, killErr)
		}
	}
	ghostCleanupWorktree(data, title)
}

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sessions",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		var allData []session.InstanceData
		if repoID != "" {
			raw, err := config.LoadRepoInstances(repoID)
			if err != nil {
				return jsonError(err)
			}
			if err := json.Unmarshal(raw, &allData); err != nil {
				return jsonError(fmt.Errorf("failed to parse instances: %w", err))
			}
		} else {
			// Don't silently substitute an empty/partial list when a repo
			// file is corrupted (#730): loadAllInstancesAggregate warns naming
			// each bad repo, and we fail loudly so users can tell "no sessions"
			// apart from "sessions hidden behind a corrupt file."
			data, corrupted, err := loadAllInstancesAggregate()
			if err != nil {
				return jsonError(err)
			}
			if len(corrupted) > 0 {
				return jsonError(corruptedReposError(corrupted))
			}
			allData = data
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
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		data, _, err := findInstanceByTitle(args[0])
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
)

var sessionsCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new session",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// resolveRepo already differentiates "--repo is required" (absent) from a
		// provided-but-invalid path and names the offending path (#892), so
		// surface its error verbatim instead of relabeling every failure as
		// "required".
		repo, err := resolveRepo()
		if err != nil {
			return jsonError(err)
		}

		if !git.IsGitRepo(repo.Root) {
			return jsonError(fmt.Errorf("path %s is not a git repository", repo.Root))
		}

		// Best-effort per-repo pre-check to fail fast on duplicate titles
		// before we spend time creating a tmux session and git worktree we'd
		// just have to tear down. The authoritative race-safe check still
		// happens inside appendInstanceFn under the per-repo file lock.
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

		data, err := createSessionViaDaemon(daemon.CreateSessionRequest{
			Title:    createNameFlag,
			RepoPath: repo.Root,
			Program:  program,
			Prompt:   createPromptFlag,
			AutoYes:  cfg.AutoYes,
		})
		if err != nil {
			return jsonError(err)
		}

		return jsonOut(data)
	},
}

// appendInstanceFn returns a callback for config.UpdateRepoInstances that
// appends data to the existing instances array. If the existing file is
// corrupted, it returns an error so the update is aborted without
// overwriting the on-disk file (preserving it for manual recovery).
//
// If an entry with the same Title already exists, it returns an error
// rather than appending a duplicate or silently replacing the existing
// entry. Title-based lookup (findInstanceByTitle) assumes uniqueness, and
// silently replacing would orphan the prior tmux session/worktree without
// cleanup. Erroring out makes the conflict explicit to the caller, who is
// then responsible for cleaning up the just-created instance (typically
// via instance.Kill()).
func appendInstanceFn(data session.InstanceData) func(json.RawMessage) (json.RawMessage, error) {
	return func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("failed to parse existing instances: %w", err)
		}
		for i := range existing {
			if existing[i].Title == data.Title {
				return nil, fmt.Errorf("session with title %q already exists", data.Title)
			}
		}
		existing = append(existing, data)
		return json.MarshalIndent(existing, "", "  ")
	}
}

func killSessionDirect(title string) error {
	instance, repoID, err := findLiveInstanceByTitle(title)
	if err != nil {
		data, ghostRepoID, lookupErr := findInstanceByTitle(title)
		if lookupErr != nil {
			return err
		}

		ghostCleanup(data, title)

		state := config.LoadState()
		storage, storageErr := session.NewStorage(state, ghostRepoID)
		if storageErr != nil {
			return storageErr
		}
		if delErr := storage.DeleteInstance(title); delErr != nil {
			return fmt.Errorf("failed to delete instance from storage: %w", delErr)
		}
		return nil
	}

	if err := instance.Kill(); err != nil {
		return fmt.Errorf("failed to kill instance: %w", err)
	}

	state := config.LoadState()
	storage, err := session.NewStorage(state, repoID)
	if err != nil {
		return err
	}
	if err := storage.DeleteInstance(title); err != nil {
		log.ErrorLog.Printf("failed to delete instance from storage: %v", err)
	}
	return nil
}

var (
	sendPromptCreateFlag  bool
	sendPromptProgramFlag string
)

var sessionsSendPromptCmd = &cobra.Command{
	Use:   "send-prompt <title> <prompt>",
	Short: "Send a prompt to a session",
	Long: `Send a prompt to an existing session. The session must already exist unless --create is used.

If the session does not exist, use --create to automatically create it first,
or use 'af sessions create --name <title> --prompt <prompt>' instead.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

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
			return jsonError(fmt.Errorf("instance %q not found. Use --create to auto-create the session, or run: af sessions create --name %q --prompt <prompt>", title, title))
		}

		if err := sendPromptViaDaemon(daemon.SendPromptRequest{Title: title, RepoID: repoID, Prompt: prompt}); err != nil {
			return jsonError(err)
		}
		return jsonOut(map[string]bool{"ok": true})
	},
}

var sessionsPreviewCmd = &cobra.Command{
	Use:   "preview <title>",
	Short: "Preview a session's terminal content",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		instance, _, err := findLiveInstanceByTitle(args[0])
		if err != nil {
			return jsonError(err)
		}

		content, err := instance.Preview()
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
	Short: "Kill a session",
	Args:  cobra.ExactArgs(1),
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

		if err := killSessionViaDaemon(daemon.KillSessionRequest{Title: args[0], RepoID: repoID}); err != nil {
			return jsonError(err)
		}

		return jsonOut(map[string]bool{"ok": true})
	},
}

var sessionsAttachCmd = &cobra.Command{
	Use:   "attach <title>",
	Short: "Attach to a session's terminal",
	Long:  "Attach to a running session's tmux terminal. Detach with the configured detach key (default: Ctrl-b d).",
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

		detached, err := instance.Attach()
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
	Long:  "Returns the session info for the current tmux session by matching the tmux session name against stored instances.",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// Get the current tmux session name
		out, err := exec.Command("tmux", "display-message", "-p", "#{session_name}").Output()
		if err != nil {
			return jsonError(fmt.Errorf("not running inside a tmux session: %w", err))
		}
		tmuxName := strings.TrimSpace(string(out))
		if tmuxName == "" {
			return jsonError(fmt.Errorf("could not determine tmux session name"))
		}

		// Scan all instances for a matching tmux_name
		allInstances, err := config.LoadAllRepoInstances()
		if err != nil {
			return jsonError(fmt.Errorf("failed to load instances: %w", err))
		}

		var corrupted []string
		for repoID, raw := range allInstances {
			var instances []session.InstanceData
			if err := json.Unmarshal(raw, &instances); err != nil {
				// Warn and record rather than silently skip (#730): the
				// current session's record may live in the corrupted repo,
				// so a bare "not found" would mask the real cause.
				log.WarningLog.Printf("skipping repo %s: corrupted instances.json: %v", repoID, err)
				corrupted = append(corrupted, repoID)
				continue
			}
			for i := range instances {
				if instances[i].TmuxName == tmuxName {
					return jsonOut(instances[i])
				}
			}
		}

		if len(corrupted) > 0 {
			return jsonError(fmt.Errorf("no Agent Factory session found for tmux session %q; %s", tmuxName, corruptedReposSuffix(corrupted)))
		}
		return jsonError(fmt.Errorf("no Agent Factory session found for tmux session %q", tmuxName))
	},
}
