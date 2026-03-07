package schedule

import (
	"claude-squad/config"
	"fmt"
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

// ScheduleCmd is the parent command for schedule management
var ScheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Manage scheduled tasks",
	Long:  "Create, list, and manage recurring scheduled tasks that run automatically.",
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all scheduled tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		schedules, err := LoadSchedules()
		if err != nil {
			return fmt.Errorf("failed to load schedules: %w", err)
		}

		if len(schedules) == 0 {
			fmt.Println("No schedules found.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tENABLED\tCRON\tPROGRAM\tPATH\tLAST_RUN\tSTATUS")
		for _, s := range schedules {
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
	Short: "Add a new scheduled task",
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
		s := Schedule{
			ID:          id,
			Name:        nameFlag,
			Prompt:      promptFlag,
			CronExpr:    cronFlag,
			ProjectPath: absPath,
			Program:     program,
			Enabled:     true,
			CreatedAt:   time.Now(),
		}

		if err := AddSchedule(s); err != nil {
			return fmt.Errorf("failed to add schedule: %w", err)
		}

		if err := InstallSystemdTimer(s); err != nil {
			return fmt.Errorf("failed to install systemd timer: %w", err)
		}

		fmt.Printf("Schedule added successfully (ID: %s)\n", id)
		return nil
	},
}

var removeCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove a scheduled task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := GetSchedule(args[0])
		if err != nil {
			return fmt.Errorf("failed to get schedule: %w", err)
		}

		if err := RemoveSystemdTimer(*s); err != nil {
			return fmt.Errorf("failed to remove systemd timer: %w", err)
		}

		if err := RemoveSchedule(args[0]); err != nil {
			return fmt.Errorf("failed to remove schedule: %w", err)
		}

		fmt.Printf("Schedule removed successfully (ID: %s)\n", args[0])
		return nil
	},
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a scheduled task (called by systemd)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunScheduledTask(args[0])
	},
}

func init() {
	addCmd.Flags().StringVar(&nameFlag, "name", "", "Schedule name")
	addCmd.Flags().StringVar(&promptFlag, "prompt", "", "Prompt to send to the AI agent (required)")
	addCmd.Flags().StringVar(&cronFlag, "cron", "", "Cron expression for scheduling (required)")
	addCmd.Flags().StringVar(&pathFlag, "path", ".", "Project path (defaults to current directory)")
	addCmd.Flags().StringVar(&programFlag, "program", "", "Program to run (defaults to config default)")
	addCmd.MarkFlagRequired("prompt")
	addCmd.MarkFlagRequired("cron")

	ScheduleCmd.AddCommand(listCmd)
	ScheduleCmd.AddCommand(addCmd)
	ScheduleCmd.AddCommand(removeCmd)
	ScheduleCmd.AddCommand(runCmd)
}
