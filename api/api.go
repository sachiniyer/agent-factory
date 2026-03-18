package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/spf13/cobra"
)

// Shared flags
var (
	repoFlag   string
	repoIDFlag string
)

// resolveRepoID resolves a repo ID from flags, cwd, or returns "" for all-repo mode.
func resolveRepoID() (string, error) {
	if repoFlag != "" {
		absPath, err := filepath.Abs(repoFlag)
		if err != nil {
			return "", fmt.Errorf("failed to resolve repo path: %w", err)
		}
		repo, err := config.RepoFromPath(absPath)
		if err != nil {
			return "", fmt.Errorf("failed to get repo from path: %w", err)
		}
		return repo.ID, nil
	}
	if repoIDFlag != "" {
		return repoIDFlag, nil
	}
	// Try cwd
	repo, err := config.CurrentRepo()
	if err != nil {
		return "", nil // all-repo mode
	}
	return repo.ID, nil
}

// resolveRepo resolves a *config.RepoContext from flags. Returns error if no repo specified and cwd is not a repo.
func resolveRepo() (*config.RepoContext, error) {
	if repoFlag != "" {
		absPath, err := filepath.Abs(repoFlag)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve repo path: %w", err)
		}
		return config.RepoFromPath(absPath)
	}
	return config.CurrentRepo()
}

// findInstanceByTitle scans all repos for an instance matching the given title.
// Returns the InstanceData and the repoID it belongs to.
func findInstanceByTitle(title string) (*session.InstanceData, string, error) {
	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return nil, "", fmt.Errorf("failed to load instances: %w", err)
	}

	for repoID, raw := range allInstances {
		var instances []session.InstanceData
		if err := json.Unmarshal(raw, &instances); err != nil {
			continue
		}
		for i := range instances {
			if instances[i].Title == title {
				return &instances[i], repoID, nil
			}
		}
	}
	return nil, "", fmt.Errorf("instance %q not found", title)
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

// ApiCmd is the parent command for the programmatic API
var ApiCmd = &cobra.Command{
	Use:   "api",
	Short: "Programmatic JSON API for external orchestrators",
	Long:  "Machine-readable CLI interface for driving agent-factory sessions, tasks, and board.",
}

func init() {
	// Persistent flags on ApiCmd (available to all subcommands)
	ApiCmd.PersistentFlags().StringVar(&repoFlag, "repo", "", "Path to git repository")
	ApiCmd.PersistentFlags().StringVar(&repoIDFlag, "repo-id", "", "Repository ID (alternative to --repo)")

	// Sessions
	sessionsCreateCmd.Flags().StringVar(&createNameFlag, "name", "", "Session name (required)")
	sessionsCreateCmd.Flags().StringVar(&createPromptFlag, "prompt", "", "Initial prompt to send")
	sessionsCreateCmd.Flags().StringVar(&createProgramFlag, "program", "", "Program to run (defaults to config default)")
	sessionsCreateCmd.MarkFlagRequired("name")

	sessionsSendPromptCmd.Flags().BoolVar(&sendPromptCreateFlag, "create", false, "Auto-create the session if it doesn't exist")
	sessionsSendPromptCmd.Flags().StringVar(&sendPromptProgramFlag, "program", "", "Program to run when creating a new session (defaults to config default)")

	sessionsCmd.AddCommand(sessionsListCmd)
	sessionsCmd.AddCommand(sessionsGetCmd)
	sessionsCmd.AddCommand(sessionsCreateCmd)
	sessionsCmd.AddCommand(sessionsSendPromptCmd)
	sessionsCmd.AddCommand(sessionsPreviewCmd)
	sessionsCmd.AddCommand(sessionsKillCmd)

	// Tasks
	tasksAddCmd.Flags().StringVar(&taskAddNameFlag, "name", "", "Task name (required)")
	tasksAddCmd.Flags().StringVar(&taskAddPromptFlag, "prompt", "", "Prompt to send (required)")
	tasksAddCmd.Flags().StringVar(&taskAddCronFlag, "cron", "", "Cron expression (required)")
	tasksAddCmd.Flags().StringVar(&taskAddProgramFlag, "program", "", "Program to run (defaults to config default)")
	tasksAddCmd.MarkFlagRequired("name")
	tasksAddCmd.MarkFlagRequired("prompt")
	tasksAddCmd.MarkFlagRequired("cron")

	tasksCmd.AddCommand(tasksListCmd)
	tasksCmd.AddCommand(tasksAddCmd)
	tasksCmd.AddCommand(tasksRemoveCmd)

	// Board
	boardAddCmd.Flags().StringVar(&boardAddTitleFlag, "title", "", "Task title (required)")
	boardAddCmd.Flags().StringVar(&boardAddStatusFlag, "status", "backlog", "Task status column (backlog, in_progress, review, done)")
	boardAddCmd.Flags().StringVar(&boardAddInstanceFlag, "instance", "", "Link task to a session by title")
	boardAddCmd.MarkFlagRequired("title")

	boardLinkCmd.Flags().StringVar(&boardLinkInstanceFlag, "instance", "", "Session title to link (required)")
	boardLinkCmd.MarkFlagRequired("instance")

	boardMoveCmd.Flags().StringVar(&boardMoveStatusFlag, "status", "", "Target column (backlog, in_progress, review, done)")
	boardMoveCmd.MarkFlagRequired("status")

	boardSpawnCmd.Flags().StringVar(&boardSpawnProgramFlag, "program", "", "Program to run (defaults to config default)")
	boardSpawnCmd.Flags().StringVar(&boardSpawnNameFlag, "name", "", "Session name (defaults to task-<id>)")

	boardCmd.AddCommand(boardListCmd)
	boardCmd.AddCommand(boardAddCmd)
	boardCmd.AddCommand(boardToggleCmd)
	boardCmd.AddCommand(boardRemoveCmd)
	boardCmd.AddCommand(boardMoveCmd)
	boardCmd.AddCommand(boardLinkCmd)
	boardCmd.AddCommand(boardUnlinkCmd)
	boardCmd.AddCommand(boardViewCmd)
	boardCmd.AddCommand(boardSpawnCmd)

	// Register subcommand groups
	ApiCmd.AddCommand(sessionsCmd)
	ApiCmd.AddCommand(tasksCmd)
	ApiCmd.AddCommand(boardCmd)
}
