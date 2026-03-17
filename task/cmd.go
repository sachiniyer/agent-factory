package task

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/config"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var (
	nameFlag    string
	promptFlag  string
	cronFlag    string
	pathFlag    string
	programFlag string
)

// TaskCmd is the parent command for task management
var TaskCmd = &cobra.Command{
	Use:   "task",
	Short: "Manage automated tasks",
	Long:  "Create, list, and manage recurring automated tasks that run automatically.",
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all automated tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		tasks, err := LoadTasks()
		if err != nil {
			return fmt.Errorf("failed to load tasks: %w", err)
		}

		if len(tasks) == 0 {
			fmt.Println("No tasks found.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tENABLED\tCRON\tPROGRAM\tPATH\tLAST_RUN\tSTATUS")
		for _, s := range tasks {
			lastRun := "never"
			if s.LastRunAt != nil {
				lastRun = s.LastRunAt.Format(time.RFC822)
			}
			fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\t%s\t%s\t%s\n",
				s.ID, s.Name, s.Enabled, s.CronExpr, s.Program, s.ProjectPath, lastRun, s.LastRunStatus)
		}
		w.Flush()
		return nil
	},
}

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a new automated task",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := ValidateCronExpr(cronFlag); err != nil {
			return fmt.Errorf("invalid cron expression: %w", err)
		}

		absPath, err := filepath.Abs(pathFlag)
		if err != nil {
			return fmt.Errorf("failed to resolve path: %w", err)
		}

		program := programFlag
		if program == "" {
			program = config.LoadConfig().DefaultProgram
		}

		id := GenerateID()
		s := Task{
			ID:          id,
			Name:        nameFlag,
			Prompt:      promptFlag,
			CronExpr:    cronFlag,
			ProjectPath: absPath,
			Program:     program,
			Enabled:     true,
			CreatedAt:   time.Now(),
		}

		if err := AddTask(s); err != nil {
			return fmt.Errorf("failed to add task: %w", err)
		}

		if err := InstallScheduler(s); err != nil {
			return fmt.Errorf("failed to install task scheduler: %w", err)
		}

		fmt.Printf("Task added successfully (ID: %s)\n", id)
		return nil
	},
}

var removeCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove an automated task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := GetTask(args[0])
		if err != nil {
			return fmt.Errorf("failed to get task: %w", err)
		}

		if err := RemoveScheduler(*s); err != nil {
			return fmt.Errorf("failed to remove task scheduler: %w", err)
		}

		if err := RemoveTask(args[0]); err != nil {
			return fmt.Errorf("failed to remove task: %w", err)
		}

		fmt.Printf("Task removed successfully (ID: %s)\n", args[0])
		return nil
	},
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run an automated task (called by scheduler)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunTask(args[0])
	},
}

func init() {
	addCmd.Flags().StringVar(&nameFlag, "name", "", "Task name")
	addCmd.Flags().StringVar(&promptFlag, "prompt", "", "Prompt to send to the AI agent (required)")
	addCmd.Flags().StringVar(&cronFlag, "cron", "", "Cron expression (required)")
	addCmd.Flags().StringVar(&pathFlag, "path", ".", "Project path (defaults to current directory)")
	addCmd.Flags().StringVar(&programFlag, "program", "", "Program to run (defaults to config default)")
	addCmd.MarkFlagRequired("prompt")
	addCmd.MarkFlagRequired("cron")

	TaskCmd.AddCommand(listCmd)
	TaskCmd.AddCommand(addCmd)
	TaskCmd.AddCommand(removeCmd)
	TaskCmd.AddCommand(runCmd)
}
