package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// Shared flags
var (
	repoFlag string
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
		return nil, "", fmt.Errorf("failed to load instances: %w", err)
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
		return nil, "", fmt.Errorf("instance %q not found; %s", title, corruptedReposSuffix(corrupted))
	}
	// Wrap the sentinel so a clean miss stays distinguishable from a
	// corruption-tainted miss (#861); the user-facing text is unchanged.
	return nil, "", fmt.Errorf("instance %q %w", title, errTitleNotFound)
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

// loadAllInstancesAggregate aggregates instances across every repo, returning
// the parsed entries plus the IDs of repos whose instances.json failed to
// parse. Corrupted repos are logged (naming the repo) and reported via the
// second return value so callers surface them instead of silently returning a
// truncated list (#730). Empty/new repos parse cleanly to zero entries and are
// not treated as corruption, preserving backward-compatible empty results.
func loadAllInstancesAggregate() ([]session.InstanceData, []string, error) {
	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return nil, nil, err
	}
	var allData []session.InstanceData
	var corrupted []string
	for repoID, raw := range allInstances {
		var instances []session.InstanceData
		if err := json.Unmarshal(raw, &instances); err != nil {
			log.WarningLog.Printf("skipping repo %s: corrupted instances.json: %v", repoID, err)
			corrupted = append(corrupted, repoID)
			continue
		}
		allData = append(allData, instances...)
	}
	return allData, corrupted, nil
}

func repoHasInstanceTitle(repoID, title string) (bool, error) {
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		return false, fmt.Errorf("failed to load instances for repo %s: %w", repoID, err)
	}
	var instances []session.InstanceData
	if err := json.Unmarshal(raw, &instances); err != nil {
		return false, fmt.Errorf("failed to parse instances for repo %s: %w", repoID, err)
	}
	for i := range instances {
		if instances[i].Title == title {
			return true, nil
		}
	}
	return false, nil
}

// findLiveInstanceByTitle finds an instance by title and restores it as a live *Instance.
func findLiveInstanceByTitle(title string) (*session.Instance, string, error) {
	data, repoID, err := findInstanceByTitle(title)
	if err != nil {
		return nil, "", err
	}
	instance, err := session.FromInstanceData(*data)
	if err != nil {
		return nil, "", fmt.Errorf("failed to restore instance %q: %w", title, err)
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
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to load instances for repo %s: %w", repoID, err)
	}
	var instances []session.InstanceData
	if err := json.Unmarshal(raw, &instances); err != nil {
		return nil, "", fmt.Errorf("failed to parse instances for repo %s: %w", repoID, err)
	}
	for i := range instances {
		if instances[i].Title == title {
			return &instances[i], repoID, nil
		}
	}
	// Wrap the sentinel so a scoped clean miss stays distinguishable from a
	// corruption-tainted miss, mirroring findInstanceByTitle (#861).
	return nil, "", fmt.Errorf("instance %q %w", title, errTitleNotFound)
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
		return nil, "", fmt.Errorf("failed to restore instance %q: %w", title, err)
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
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		return nil, fmt.Errorf("failed to load instances for repo %s: %w", repoID, err)
	}
	var instances []session.InstanceData
	if err := json.Unmarshal(raw, &instances); err != nil {
		return nil, fmt.Errorf("failed to parse instances for repo %s: %w", repoID, err)
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

// jsonOut marshals v to JSON and writes to stdout.
func jsonOut(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// jsonError writes a JSON error to stderr and returns the error.
func jsonError(err error) error {
	msg, _ := json.Marshal(map[string]string{"error": err.Error()})
	fmt.Fprintln(os.Stderr, string(msg))
	return err
}

func init() {
	// --repo flag on each top-level subcommand
	SessionsCmd.PersistentFlags().StringVar(&repoFlag, "repo", "", "Path to git repository")
	TasksCmd.PersistentFlags().StringVar(&repoFlag, "repo", "", "Path to git repository")

	// Sessions
	sessionsCreateCmd.Flags().StringVar(&createNameFlag, "name", "", "Session name (required)")
	sessionsCreateCmd.Flags().StringVar(&createPromptFlag, "prompt", "", "Initial prompt to send")
	sessionsCreateCmd.Flags().StringVar(&createProgramFlag, "program", "", "Program to run (defaults to config default)")
	sessionsCreateCmd.Flags().BoolVar(&createHereFlag, "here", false, "Run in the repo's existing working tree at its current branch (no new worktree/branch; kill preserves both)")
	sessionsCreateCmd.Flags().BoolVar(&createInPlaceFlag, "in-place", false, "Alias for --here")
	sessionsCreateCmd.MarkFlagRequired("name")

	sessionsSendPromptCmd.Flags().BoolVar(&sendPromptCreateFlag, "create", false, "Auto-create the session if it doesn't exist")
	sessionsSendPromptCmd.Flags().StringVar(&sendPromptProgramFlag, "program", "", "Program to run when creating a new session (defaults to config default)")
	sessionsSendPromptCmd.Flags().BoolVar(&sendPromptAllFlag, "all", false, "Broadcast the prompt to every live session in scope (current repo by default; excludes the reserved root session)")
	sessionsSendPromptCmd.Flags().BoolVar(&sendPromptAllReposFlag, "all-repos", false, "With --all, broadcast across every repo instead of only the current/--repo one")
	sessionsSendPromptCmd.Flags().BoolVar(&sendPromptIncludeRootFlag, "include-root", false, "With --all, also deliver to the reserved root session (excluded by default)")

	sessionsTabCreateCmd.Flags().StringVar(&tabCreateCommandFlag, "command", "", "Command to run in the new tab (required)")
	sessionsTabCreateCmd.Flags().StringVar(&tabCreateNameFlag, "name", "", "Tab name (defaults to the command basename; auto-suffixed on collision)")
	sessionsTabCreateCmd.MarkFlagRequired("command")

	sessionsTabDeleteCmd.Flags().StringVar(&tabDeleteNameFlag, "name", "", "Name of the tab to delete (required)")
	sessionsTabDeleteCmd.MarkFlagRequired("name")

	SessionsCmd.AddCommand(sessionsListCmd)
	SessionsCmd.AddCommand(sessionsGetCmd)
	SessionsCmd.AddCommand(sessionsCreateCmd)
	SessionsCmd.AddCommand(sessionsSendPromptCmd)
	SessionsCmd.AddCommand(sessionsTabCreateCmd)
	SessionsCmd.AddCommand(sessionsTabDeleteCmd)
	SessionsCmd.AddCommand(sessionsPreviewCmd)
	SessionsCmd.AddCommand(sessionsKillCmd)
	SessionsCmd.AddCommand(sessionsAttachCmd)
	SessionsCmd.AddCommand(sessionsWhoamiCmd)

	// Tasks
	tasksAddCmd.Flags().StringVar(&taskAddNameFlag, "name", "", "Task name (required)")
	tasksAddCmd.Flags().StringVar(&taskAddPromptFlag, "prompt", "", "Prompt to send (required for --cron tasks; --watch-cmd tasks default to the emitted line, with {{line}} substituted when present)")
	tasksAddCmd.Flags().StringVar(&taskAddCronFlag, "cron", "", "Cron expression (exactly one of --cron / --watch-cmd)")
	tasksAddCmd.Flags().StringVar(&taskAddWatchCmdFlag, "watch-cmd", "", "Long-running watch command; each stdout line triggers the task (exactly one of --cron / --watch-cmd)")
	tasksAddCmd.Flags().StringVar(&taskAddTargetSessionFlag, "target-session", "", "Deliver the prompt into this session (auto-created if missing); empty creates a new session per run")
	tasksAddCmd.Flags().StringVar(&taskAddProgramFlag, "program", "", "Program to run (defaults to config default)")
	tasksAddCmd.MarkFlagRequired("name")

	tasksUpdateCmd.Flags().StringVar(&taskUpdateNameFlag, "name", "", "New task name")
	tasksUpdateCmd.Flags().StringVar(&taskUpdatePromptFlag, "prompt", "", "New prompt")
	tasksUpdateCmd.Flags().StringVar(&taskUpdateCronFlag, "cron", "", "New cron expression (clears watch-cmd)")
	tasksUpdateCmd.Flags().StringVar(&taskUpdateWatchCmdFlag, "watch-cmd", "", "New watch command (clears cron)")
	tasksUpdateCmd.Flags().StringVar(&taskUpdateTargetSessionFlag, "target-session", "", "New target session; pass an empty value to revert to a new session per run")
	tasksUpdateCmd.Flags().StringVar(&taskUpdateEnabledFlag, "enabled", "", "Enable or disable the task (true/false)")
	tasksUpdateCmd.Flags().StringVar(&taskUpdateProgramFlag, "program", "", "New program to run (leave unset to keep the current one)")

	TasksCmd.AddCommand(tasksListCmd)
	TasksCmd.AddCommand(tasksGetCmd)
	TasksCmd.AddCommand(tasksAddCmd)
	TasksCmd.AddCommand(tasksUpdateCmd)
	TasksCmd.AddCommand(tasksRemoveCmd)
	TasksCmd.AddCommand(tasksRunCmd)
}
