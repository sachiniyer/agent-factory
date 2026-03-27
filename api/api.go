package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// Shared flags
var (
	repoFlag string
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

func init() {
	// --repo flag on each top-level subcommand
	SessionsCmd.PersistentFlags().StringVar(&repoFlag, "repo", "", "Path to git repository")
	TasksCmd.PersistentFlags().StringVar(&repoFlag, "repo", "", "Path to git repository")

	// Sessions
	sessionsCreateCmd.Flags().StringVar(&createNameFlag, "name", "", "Session name (required)")
	sessionsCreateCmd.Flags().StringVar(&createPromptFlag, "prompt", "", "Initial prompt to send")
	sessionsCreateCmd.Flags().StringVar(&createProgramFlag, "program", "", "Program to run (defaults to config default)")
	sessionsCreateCmd.MarkFlagRequired("name")

	sessionsSendPromptCmd.Flags().BoolVar(&sendPromptCreateFlag, "create", false, "Auto-create the session if it doesn't exist")
	sessionsSendPromptCmd.Flags().StringVar(&sendPromptProgramFlag, "program", "", "Program to run when creating a new session (defaults to config default)")

	SessionsCmd.AddCommand(sessionsListCmd)
	SessionsCmd.AddCommand(sessionsGetCmd)
	SessionsCmd.AddCommand(sessionsCreateCmd)
	SessionsCmd.AddCommand(sessionsSendPromptCmd)
	SessionsCmd.AddCommand(sessionsPreviewCmd)
	SessionsCmd.AddCommand(sessionsKillCmd)
	SessionsCmd.AddCommand(sessionsAttachCmd)
	SessionsCmd.AddCommand(sessionsWhoamiCmd)

	// Tasks
	tasksAddCmd.Flags().StringVar(&taskAddNameFlag, "name", "", "Task name (required)")
	tasksAddCmd.Flags().StringVar(&taskAddPromptFlag, "prompt", "", "Prompt to send (required)")
	tasksAddCmd.Flags().StringVar(&taskAddCronFlag, "cron", "", "Cron expression (required)")
	tasksAddCmd.Flags().StringVar(&taskAddProgramFlag, "program", "", "Program to run (defaults to config default)")
	tasksAddCmd.MarkFlagRequired("name")
	tasksAddCmd.MarkFlagRequired("prompt")
	tasksAddCmd.MarkFlagRequired("cron")

	tasksUpdateCmd.Flags().StringVar(&taskUpdateNameFlag, "name", "", "New task name")
	tasksUpdateCmd.Flags().StringVar(&taskUpdatePromptFlag, "prompt", "", "New prompt")
	tasksUpdateCmd.Flags().StringVar(&taskUpdateCronFlag, "cron", "", "New cron expression")
	tasksUpdateCmd.Flags().StringVar(&taskUpdateEnabledFlag, "enabled", "", "Enable or disable the task (true/false)")

	TasksCmd.AddCommand(tasksListCmd)
	TasksCmd.AddCommand(tasksGetCmd)
	TasksCmd.AddCommand(tasksAddCmd)
	TasksCmd.AddCommand(tasksUpdateCmd)
	TasksCmd.AddCommand(tasksRemoveCmd)
	TasksCmd.AddCommand(tasksRunCmd)
}
