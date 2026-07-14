package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// snapshotRead runs the non-spawning daemon snapshot and reports whether the
// caller may fall back to a local disk scan. It is the single seam every
// daemon→disk read path (listSessions, getSessionByTitle, whoamiSession,
// getSessionByTitleInScope) routes through so the "remote reads never fall back
// to disk" contract cannot be silently reintroduced at a new read site (#1679,
// #1681). On daemon success it returns (data, false, nil). On error it returns:
//   - remote target: (nil, false, err) — surface the real error; a remote daemon
//     has no local disk to fall back to, and a bad token must not be masked by a
//     same-machine disk read (docs/remote-tcp-auth.md, #1592 Phase 3 PR4).
//   - local target:  (nil, true, err)  — the caller runs its own disk scan.
//
// Callers switch on err == nil for the success path and, on error, consult the
// fallBackToDisk flag before touching disk. This keeps each caller's exact local
// disk-fallback behavior (diskListSessions / diskWhoami / findInstanceByTitle,
// including their distinct not-found and corrupt-repo errors) while centralizing
// the one decision that must never regress: whether disk may be read at all.
func snapshotRead(req daemon.SnapshotRequest) (data []session.InstanceData, fallBackToDisk bool, err error) {
	data, err = snapshotViaDaemon(req)
	if err == nil {
		return data, false, nil
	}
	if apiclient.IsRemoteTarget() {
		return nil, false, err
	}
	return nil, true, err
}

// Shared flags
var (
	repoFlag string
	// envelopeOutput is set by the opt-in --json flag on the sessions/tasks
	// command groups. It defaults OFF so every existing invocation is
	// byte-identical to today (bare payload / {"error":"<msg>"}); when ON,
	// jsonOut/jsonError wrap output in the shared {data,error} Envelope (#1029).
	envelopeOutput bool
)

// repoFromFlag resolves the --repo flag to a RepoContext. Its errors name the
// offending path and distinguish "could not make the path absolute" from "the
// path is not a git repository" so callers never mislabel a provided-but-invalid
// --repo as missing (#892). Only call when repoFlag != "".
func repoFromFlag() (*config.RepoContext, error) {
	absPath, err := filepath.Abs(repoFlag)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve --repo path %q: %w", repoFlag, err)
	}
	repo, err := config.RepoFromPath(absPath)
	if err != nil {
		return nil, fmt.Errorf("--repo %q is not a valid git repository: %w", absPath, err)
	}
	return repo, nil
}

// resolveRepoID resolves a repo ID from flags, cwd, or returns "" for all-repo mode.
func resolveRepoID() (string, error) {
	if repoFlag != "" {
		repo, err := repoFromFlag()
		if err != nil {
			return "", err
		}
		return repo.ID, nil
	}
	// Try cwd
	repo, err := config.CurrentRepo()
	if err != nil {
		return "", nil // all-repo mode
	}
	return repo.ID, nil
}

// resolveRepo resolves a *config.RepoContext for commands that require a repo
// (sessions create, tasks add). It returns fully-formed, user-facing errors so
// callers can surface them directly without re-deriving the cause: a provided
// `--repo` that does not resolve names the path, while an absent `--repo` whose
// cwd is also not a repo reports that `--repo is required` (#892). Wrapping any
// resolution failure as "--repo is required" — as the callers used to — was
// wrong precisely when the user did pass `--repo` but pointed it at a non-repo.
func resolveRepo() (*config.RepoContext, error) {
	if repoFlag != "" {
		return repoFromFlag()
	}
	repo, err := config.CurrentRepo()
	if err != nil {
		return nil, fmt.Errorf("--repo is required: current directory is not a git repository: %w", err)
	}
	return repo, nil
}

// errTitleNotFound marks a definitive not-found from findInstanceByTitle: the
// title matched no instance and every repo's instances.json parsed cleanly. A
// corruption-tainted search returns a different (un-wrapped) error so callers
// like the send-prompt pre-check can tell "not present anywhere" apart from
// "may be hidden behind a corrupted instances.json" and surface the latter
// loudly instead of a misleading bare not-found (#861, follow-up to #730/#752).
var errTitleNotFound = errors.New("not found")

// findInstanceByTitle scans all repos for an instance matching the given title.
// Returns the InstanceData and the repoID it belongs to.
func findInstanceByTitle(title string) (*session.InstanceData, string, error) {
	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return nil, "", fmt.Errorf("failed to load sessions: %w", err)
	}

	var corrupted []string
	for repoID, raw := range allInstances {
		var instances []session.InstanceData
		if err := json.Unmarshal(raw, &instances); err != nil {
			// Warn and record the corrupted repo rather than silently
			// skipping it (#730). If the target title lives in this repo we
			// would otherwise report a misleading "not found".
			log.WarningLog.Printf("skipping repo %s: corrupted instances.json: %v", repoID, err)
			corrupted = append(corrupted, repoID)
			continue
		}
		for i := range instances {
			if instances[i].Title == title {
				return &instances[i], repoID, nil
			}
		}
	}
	if len(corrupted) > 0 {
		return nil, "", fmt.Errorf("session %q not found; %s", title, corruptedReposSuffix(corrupted))
	}
	// Wrap the sentinel so a clean miss stays distinguishable from a
	// corruption-tainted miss (#861); the user-facing text is unchanged.
	return nil, "", fmt.Errorf("session %q %w", title, errTitleNotFound)
}

// corruptedReposSuffix builds a sorted, human-readable clause naming the repos
// whose instances.json failed to parse. Callers use it to surface corruption
// loudly instead of silently returning empty/partial results (#730).
func corruptedReposSuffix(corrupted []string) string {
	sort.Strings(corrupted)
	return fmt.Sprintf("%d repo(s) have a corrupted instances.json and may be hiding it: %s", len(corrupted), strings.Join(corrupted, ", "))
}

// corruptedReposError builds a structured error for aggregate queries (e.g.
// `sessions list`) that name the repos whose instances.json failed to parse.
// Returning this instead of a silently-truncated result lets users tell "no
// sessions exist" apart from "sessions exist but the file is corrupted" (#730).
func corruptedReposError(corrupted []string) error {
	sort.Strings(corrupted)
	return fmt.Errorf("%d repo(s) have a corrupted instances.json and their sessions are hidden until it is repaired: %s", len(corrupted), strings.Join(corrupted, ", "))
}

// diskListSessions is the disk-read fallback for `sessions list` when no daemon
// is reachable (#1029 PR 2). It reproduces the pre-daemon read behavior exactly
// — repo-scoped or all-repos, keeping the loud corrupt-file error on the
// all-repos path (#730) — and additionally sorts the result by the daemon's
// (repoID, title) key so the on-disk order matches the daemon Snapshot path,
// giving scripts a stable order from either source.
func diskListSessions(repoID string) ([]session.InstanceData, error) {
	if repoID != "" {
		raw, err := config.LoadRepoInstances(repoID)
		if err != nil {
			return nil, err
		}
		var data []session.InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("failed to parse sessions: %w", err)
		}
		// Single repo: the (repoID, title) key reduces to title order.
		sort.Slice(data, func(i, j int) bool { return data[i].Title < data[j].Title })
		return data, nil
	}

	// Don't silently substitute an empty/partial list when a repo file is
	// corrupted (#730): warn naming each bad repo and fail loudly so users can
	// tell "no sessions" apart from "sessions hidden behind a corrupt file."
	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return nil, err
	}
	type keyedInstance struct {
		key  string
		data session.InstanceData
	}
	var rows []keyedInstance
	var corrupted []string
	for rid, raw := range allInstances {
		var instances []session.InstanceData
		if err := json.Unmarshal(raw, &instances); err != nil {
			log.WarningLog.Printf("skipping repo %s: corrupted instances.json: %v", rid, err)
			corrupted = append(corrupted, rid)
			continue
		}
		for _, inst := range instances {
			// Build the same composite key the daemon sorts by:
			// repoID + NUL + title (daemonInstanceKey). NUL sorts before any
			// printable byte, so this is exactly the daemon's (repoID, title)
			// order.
			rows = append(rows, keyedInstance{key: rid + "\x00" + inst.Title, data: inst})
		}
	}
	if len(corrupted) > 0 {
		return nil, corruptedReposError(corrupted)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	data := make([]session.InstanceData, 0, len(rows))
	for _, r := range rows {
		data = append(data, r.data)
	}
	return data, nil
}

// diskWhoami is the disk-read fallback for `sessions whoami` when no daemon is
// reachable (#1029 PR 2). It scans every repo for an instance whose TmuxName
// matches the current tmux session, keeping the loud corrupt-file behavior
// (#730) so a hidden match is reported instead of a misleading "not found".
func diskWhoami(tmuxName string) (*session.InstanceData, error) {
	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return nil, fmt.Errorf("failed to load sessions: %w", err)
	}
	var corrupted []string
	for repoID, raw := range allInstances {
		var instances []session.InstanceData
		if err := json.Unmarshal(raw, &instances); err != nil {
			log.WarningLog.Printf("skipping repo %s: corrupted instances.json: %v", repoID, err)
			corrupted = append(corrupted, repoID)
			continue
		}
		for i := range instances {
			if instances[i].TmuxName == tmuxName {
				return &instances[i], nil
			}
		}
	}
	if len(corrupted) > 0 {
		return nil, fmt.Errorf("no Agent Factory session found for tmux session %q; %s", tmuxName, corruptedReposSuffix(corrupted))
	}
	return nil, fmt.Errorf("no Agent Factory session found for tmux session %q", tmuxName)
}

func repoHasInstanceTitle(repoID, title string) (bool, error) {
	instances, err := loadRepoInstanceData(repoID)
	if err != nil {
		return false, err
	}
	for i := range instances {
		if instances[i].Title == title {
			return true, nil
		}
	}
	return false, nil
}

func loadRepoInstanceData(repoID string) ([]session.InstanceData, error) {
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		return nil, fmt.Errorf("failed to load sessions for repo %s: %w", repoID, err)
	}
	var instances []session.InstanceData
	if err := json.Unmarshal(raw, &instances); err != nil {
		return nil, fmt.Errorf("failed to parse sessions for repo %s: %w", repoID, err)
	}
	return instances, nil
}

// findLiveInstanceByTitle finds an instance by title and restores it as a live *Instance.
func findLiveInstanceByTitle(title string) (*session.Instance, string, error) {
	data, repoID, err := findInstanceByTitle(title)
	if err != nil {
		return nil, "", err
	}
	instance, err := session.FromInstanceData(*data)
	if err != nil {
		return nil, "", fmt.Errorf("failed to restore session %q: %w", title, err)
	}
	return instance, repoID, nil
}

// findInstanceByTitleInScope finds an instance by title within the resolved repo
// scope (#891). An empty repoID preserves the prior all-repo search; a non-empty
// one confines the lookup to that repo so a same-titled session in a different
// repo can never be selected. Mirrors how resolveRepoID() scopes the other
// sessions subcommands (list, kill, send-prompt).
func findInstanceByTitleInScope(repoID, title string) (*session.InstanceData, string, error) {
	if repoID == "" {
		return findInstanceByTitle(title)
	}
	instances, err := loadRepoInstanceData(repoID)
	if err != nil {
		return nil, "", err
	}
	for i := range instances {
		if instances[i].Title == title {
			return &instances[i], repoID, nil
		}
	}
	// Wrap the sentinel so a scoped clean miss stays distinguishable from a
	// corruption-tainted miss, mirroring findInstanceByTitle (#861).
	return nil, "", fmt.Errorf("session %q %w", title, errTitleNotFound)
}

// findLiveInstanceByTitleInScope finds an instance by title within the resolved
// repo scope and restores it as a live *Instance (#891). It is the repo-scoped
// counterpart of findLiveInstanceByTitle, used by attach so `--repo` confines
// the attach to that repo's session instead of connecting the terminal to a
// same-titled session in another repo.
func findLiveInstanceByTitleInScope(repoID, title string) (*session.Instance, string, error) {
	data, repoID, err := findInstanceByTitleInScope(repoID, title)
	if err != nil {
		return nil, "", err
	}
	instance, err := session.FromInstanceData(*data)
	if err != nil {
		return nil, "", fmt.Errorf("failed to restore session %q: %w", title, err)
	}
	return instance, repoID, nil
}

// instanceTitleExistsInScope reports whether a session with the given title
// exists within the resolved repo scope (#776). An empty repoID preserves the
// prior all-repo search; a non-empty one confines the check to that repo so a
// same-titled session in a different repo can never satisfy the pre-check.
// Mirrors how resolveRepoID() scopes the other sessions subcommands (list,
// kill). This is a pure existence check: unlike findLiveInstanceByTitle it does
// not restore (and Start) the instance, since callers only need to know whether
// the title is taken in scope and the daemon does its own session restore on
// delivery.
func instanceTitleExistsInScope(repoID, title string) (bool, error) {
	if repoID != "" {
		return repoHasInstanceTitle(repoID, title)
	}
	_, _, err := findInstanceByTitle(title)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, errTitleNotFound) {
		// Definitive not-found with no corruption: report "not present" so the
		// caller drives the create-vs-friendly-error branch as before.
		return false, nil
	}
	// Corruption (or a load failure): propagate so send-prompt surfaces the
	// corruption-aware message naming the bad repo instead of a misleading bare
	// not-found (#861). The session may be hidden behind the unreadable file, so
	// even --create must not silently make a duplicate.
	return false, err
}

// scopedInstance is a persisted session paired with the repo ID it belongs to,
// which the broadcast delivery path needs to address the daemon SendPrompt RPC.
type scopedInstance struct {
	RepoID string
	Title  string
	Status session.Status
}

// scopedInstancesForRepo lists one repo's persisted sessions with their repo ID
// attached. Used by the broadcast path (send-prompt --all) to enumerate the
// current/--repo scope's targets. Mirrors repoHasInstanceTitle's load+parse but
// returns every entry rather than a single existence bit.
func scopedInstancesForRepo(repoID string) ([]scopedInstance, error) {
	instances, err := loadRepoInstanceData(repoID)
	if err != nil {
		return nil, err
	}
	out := make([]scopedInstance, 0, len(instances))
	for i := range instances {
		out = append(out, scopedInstance{RepoID: repoID, Title: instances[i].Title, Status: instances[i].Status})
	}
	return out, nil
}

// allScopedInstances lists every repo's persisted sessions, preserving the repo
// ID association each broadcast delivery needs (loadAllInstancesAggregate drops
// it). Corrupted repos are logged (naming the repo) and returned via the second
// value so the caller fails loudly instead of broadcasting to a truncated set
// (#730) — the same contract as loadAllInstancesAggregate.
func allScopedInstances() ([]scopedInstance, []string, error) {
	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return nil, nil, err
	}
	var out []scopedInstance
	var corrupted []string
	for repoID, raw := range allInstances {
		var instances []session.InstanceData
		if err := json.Unmarshal(raw, &instances); err != nil {
			log.WarningLog.Printf("skipping repo %s: corrupted instances.json: %v", repoID, err)
			corrupted = append(corrupted, repoID)
			continue
		}
		for i := range instances {
			out = append(out, scopedInstance{RepoID: repoID, Title: instances[i].Title, Status: instances[i].Status})
		}
	}
	return out, corrupted, nil
}

// jsonOut marshals v to JSON and writes to stdout. By default it prints the
// bare payload (byte-identical to before #1029). With the opt-in --json flag it
// wraps the payload in the shared success Envelope.
func jsonOut(v any) error {
	if envelopeOutput {
		return apiproto.WriteEnvelope(os.Stdout, apiproto.Success(v))
	}
	data, err := apiproto.MarshalIndented(v)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// jsonError writes a JSON error to stderr and returns the error. By default it
// prints the bare {"error":"<msg>"} form (byte-identical to before #1029). With
// the opt-in --json flag it emits the shared failure Envelope instead. The
// original error is always returned so exit codes are unchanged.
func jsonError(err error) error {
	// jsonError always prints the error itself — the bare {"error":...} or the
	// envelope — so tell cobra not to re-print it (as "Error: ...") or dump
	// usage. Without this a single runtime error prints two or three times
	// (#1749). Flag-parse errors never reach here, so their usage help is
	// unaffected.
	silenceCobraOutput()
	if envelopeOutput {
		log.CloseQuiet()
		_ = apiproto.WriteEnvelope(os.Stderr, apiproto.Failure(err.Error()))
		return err
	}
	msg, _ := json.Marshal(map[string]string{"error": err.Error()})
	fmt.Fprintln(os.Stderr, string(msg))
	return err
}

// silenceCobraOutput suppresses cobra's own error line and usage dump on the
// sessions/tasks command trees (and their shared root) so a command that has
// already reported its failure does not have it re-printed (#1749).
func silenceCobraOutput() {
	SessionsCmd.SilenceUsage = true
	SessionsCmd.SilenceErrors = true
	TasksCmd.SilenceUsage = true
	TasksCmd.SilenceErrors = true
	if root := SessionsCmd.Root(); root != nil {
		root.SilenceUsage = true
		root.SilenceErrors = true
	}
	if root := TasksCmd.Root(); root != nil {
		root.SilenceUsage = true
		root.SilenceErrors = true
	}
}

func init() {
	// --repo flag on each top-level subcommand. The flag name stays --repo (a
	// technical git term), but the help reads "project" — the user-facing noun
	// the TUI and web share for a repo's session grouping (#1749).
	SessionsCmd.PersistentFlags().StringVar(&repoFlag, "repo", "", "Path to the project's git repository")
	TasksCmd.PersistentFlags().StringVar(&repoFlag, "repo", "", "Path to the project's git repository")

	// Opt-in envelope output. Defaults OFF so existing scripts keep parsing the
	// bare payload; --json wraps stdout/stderr in the {data,error} Envelope that
	// the CLI and the later HTTP server share (#1029). Bound to both groups'
	// PersistentFlags (like --repo) so it works on every subcommand.
	const jsonFlagUsage = "Wrap output in the {data,error} JSON envelope (default: bare payload)"
	SessionsCmd.PersistentFlags().BoolVar(&envelopeOutput, "json", false, jsonFlagUsage)
	TasksCmd.PersistentFlags().BoolVar(&envelopeOutput, "json", false, jsonFlagUsage)

	// Sessions
	sessionsCreateCmd.Flags().StringVar(&createNameFlag, "name", "", "Session name (required)")
	sessionsCreateCmd.Flags().StringVar(&createPromptFlag, "prompt", "", "Initial prompt to send")
	sessionsCreateCmd.Flags().StringVar(&createProgramFlag, "program", "", "Program to run (one of: "+tmux.SupportedProgramsString()+"; defaults to config default)")
	sessionsCreateCmd.Flags().BoolVar(&createHereFlag, "here", false, "Run in the repo's existing working tree at its current branch (no new worktree/branch; kill preserves both)")
	sessionsCreateCmd.Flags().BoolVar(&createInPlaceFlag, "in-place", false, "Alias for --here")
	sessionsCreateCmd.Flags().StringVar(&createBackendFlag, "backend", "", "Runtime to run the session on (one of: "+config.BackendLocal+", "+config.BackendDocker+", "+config.BackendSSH+", "+config.BackendHook+"; defaults to the repo's backend config, or local). docker runs the session in a container (set docker.image in the repo config); ssh is not yet implemented")
	sessionsCreateCmd.MarkFlagRequired("name")

	sessionsSendPromptCmd.Flags().BoolVar(&sendPromptCreateFlag, "create", false, "Auto-create the session if it doesn't exist")
	sessionsSendPromptCmd.Flags().StringVar(&sendPromptProgramFlag, "program", "", "Program to run when creating a new session (one of: "+tmux.SupportedProgramsString()+"; defaults to config default)")
	sessionsSendPromptCmd.Flags().BoolVar(&sendPromptAllFlag, "all", false, "Broadcast the prompt to every live session in scope (current repo by default; excludes the reserved root session)")
	sessionsSendPromptCmd.Flags().BoolVar(&sendPromptAllReposFlag, "all-repos", false, "With --all, broadcast across every repo instead of only the current/--repo one")
	sessionsSendPromptCmd.Flags().BoolVar(&sendPromptIncludeRootFlag, "include-root", false, "With --all, also deliver to the reserved root session (excluded by default)")
	// --force is a deprecated no-op. Register it without binding a package
	// variable: Cobra still accepts existing scripts' flag, but no mutable state
	// can imply that it affects the always-destructive kill operation.
	sessionsKillCmd.Flags().Bool("force", false, "Deprecated no-op, accepted for compatibility: kill always destroys the session (use 'af sessions archive' to keep it restorable)")
	sessionsArchiveCmd.Flags().BoolVar(&sessionsArchiveSelf, "self", false, "Archive the current session (resolved via whoami); use from inside a session when your work is done")

	sessionsWatchCmd.Flags().DurationVar(&watchTimeoutFlag, "timeout", 30*time.Minute, "Give up and exit non-zero if the session is not idle within this window (0 = wait forever)")
	sessionsWatchCmd.Flags().DurationVar(&watchIntervalFlag, "interval", 2*time.Second, "How often to poll the session's status")

	// tab-create/tab-delete and their tabs {create,delete} aliases (#1192)
	// share the same flag globals via these binders, so the two spellings stay
	// in lockstep.
	bindTabCreateFlags(sessionsTabCreateCmd)
	bindTabCreateFlags(sessionsTabsCreateCmd)
	bindTabDeleteFlags(sessionsTabDeleteCmd)
	bindTabDeleteFlags(sessionsTabsDeleteCmd)
	sessionsTabsCmd.AddCommand(sessionsTabsCreateCmd)
	sessionsTabsCmd.AddCommand(sessionsTabsDeleteCmd)

	SessionsCmd.AddCommand(sessionsListCmd)
	SessionsCmd.AddCommand(sessionsGetCmd)
	SessionsCmd.AddCommand(sessionsCreateCmd)
	SessionsCmd.AddCommand(sessionsSendPromptCmd)
	SessionsCmd.AddCommand(sessionsTabCreateCmd)
	SessionsCmd.AddCommand(sessionsTabDeleteCmd)
	SessionsCmd.AddCommand(sessionsTabsCmd)
	SessionsCmd.AddCommand(sessionsPreviewCmd)
	SessionsCmd.AddCommand(sessionsWatchCmd)
	SessionsCmd.AddCommand(sessionsKillCmd)
	SessionsCmd.AddCommand(sessionsArchiveCmd)
	SessionsCmd.AddCommand(sessionsRestoreCmd)
	SessionsCmd.AddCommand(sessionsAttachCmd)
	SessionsCmd.AddCommand(sessionsWhoamiCmd)

	// Projects (repo groupings, #1735)
	ProjectsCmd.PersistentFlags().BoolVar(&envelopeOutput, "json", false, jsonFlagUsage)
	ProjectsCmd.AddCommand(projectsDeleteCmd)

	// Tasks
	tasksAddCmd.Flags().StringVar(&taskAddNameFlag, "name", "", "Task name (required)")
	tasksAddCmd.Flags().StringVar(&taskAddPromptFlag, "prompt", "", "Prompt to send (required for --cron tasks; --watch-cmd tasks default to the emitted line, with {{line}} substituted when present)")
	tasksAddCmd.Flags().StringVar(&taskAddCronFlag, "cron", "", "Cron expression (exactly one of --cron / --watch-cmd)")
	tasksAddCmd.Flags().StringVar(&taskAddWatchCmdFlag, "watch-cmd", "", "Long-running watch command; each stdout line triggers the task (exactly one of --cron / --watch-cmd)")
	tasksAddCmd.Flags().StringVar(&taskAddTargetSessionFlag, "target-session", "", "Deliver the prompt into this session (auto-created if missing); empty creates a new session per run")
	tasksAddCmd.Flags().StringVar(&taskAddProgramFlag, "program", "", "Program to run (one of: "+tmux.SupportedProgramsString()+"; defaults to config default)")
	tasksAddCmd.MarkFlagRequired("name")

	tasksUpdateCmd.Flags().StringVar(&taskUpdateNameFlag, "name", "", "New task name")
	tasksUpdateCmd.Flags().StringVar(&taskUpdatePromptFlag, "prompt", "", "New prompt")
	tasksUpdateCmd.Flags().StringVar(&taskUpdateCronFlag, "cron", "", "New cron expression (clears watch-cmd)")
	tasksUpdateCmd.Flags().StringVar(&taskUpdateWatchCmdFlag, "watch-cmd", "", "New watch command (clears cron)")
	tasksUpdateCmd.Flags().StringVar(&taskUpdateTargetSessionFlag, "target-session", "", "New target session; pass an empty value to revert to a new session per run")
	tasksUpdateCmd.Flags().StringVar(&taskUpdateEnabledFlag, "enabled", "", "Enable or disable the task (true/false)")
	tasksUpdateCmd.Flags().StringVar(&taskUpdateProgramFlag, "program", "", "New program to run (one of: "+tmux.SupportedProgramsString()+"; leave unset to keep the current one)")

	TasksCmd.AddCommand(tasksListCmd)
	TasksCmd.AddCommand(tasksGetCmd)
	TasksCmd.AddCommand(tasksAddCmd)
	TasksCmd.AddCommand(tasksUpdateCmd)
	TasksCmd.AddCommand(tasksRemoveCmd)
	TasksCmd.AddCommand(tasksRunCmd)
}
