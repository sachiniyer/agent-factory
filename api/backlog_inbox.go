package api

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/log"
)

var backlogInboxCmd = &cobra.Command{
	Use:   "inbox",
	Short: "Print the project's open questions, oldest first",
	Long: "Print every open question on every item of the project, oldest first, with the " +
		"agent's handoff notes when it left any. Items in archived groups are left out. Answer " +
		"with \"af backlog answer <id> <text>\". With --json, print the questions as JSON in " +
		"the {data,error} envelope.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()
		t, err := backlogProject()
		if err != nil {
			return jsonError(err)
		}
		v, err := t.get()
		if err != nil {
			return jsonError(err)
		}
		inbox := v.Backlog.Inbox()
		if envelopeOutput {
			return jsonOut(inbox)
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), formatBacklogInbox(inbox, time.Now()))
		return err
	},
}

func formatBacklogInbox(inbox []backlog.InboxEntry, now time.Time) string {
	if len(inbox) == 0 {
		return "No questions\n"
	}
	var sb strings.Builder
	for _, e := range inbox {
		fmt.Fprintf(&sb, "%s  %s  · %s ago\n", backlog.FormatID(e.ID), terminalSafe(e.Title), inboxAge(now.Sub(e.Question.At)))
		for _, line := range strings.Split(terminalSafe(e.Question.Text), "\n") {
			fmt.Fprintf(&sb, "  %s\n", line)
		}
		if e.Question.Handoff != "" {
			sb.WriteString("  Notes from the agent:\n")
			for _, line := range strings.Split(terminalSafe(e.Question.Handoff), "\n") {
				fmt.Fprintf(&sb, "    %s\n", line)
			}
		}
	}
	return sb.String()
}

// terminalSafe drops control characters other than newline and tab, so agent
// text cannot send escape sequences to the user's terminal.
func terminalSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ReplaceAll(s, "\r\n", "\n"))
}

func inboxAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

func init() {
	BacklogCmd.AddCommand(backlogInboxCmd)
}
