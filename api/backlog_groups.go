package api

import (
	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/daemon"
)

type groupChange struct {
	paused, archived *bool
}

func backlogGroupCmd(use, short, long string, change groupChange) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <id>",
		Short: short,
		Long:  long,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBacklog(func() (any, error) {
				if err := refuseFromItemSession(use); err != nil {
					return nil, err
				}
				id, err := backlog.ParseID(args[0])
				if err != nil {
					return nil, err
				}
				t, err := backlogProject()
				if err != nil {
					return nil, err
				}
				var resp daemon.UpdateBacklogItemResponse
				if err := callBacklogRoute("UpdateBacklogItem", daemon.UpdateBacklogItemRequest{
					RepoID: t.repoID, RepoPath: t.repoPath, ID: id, Paused: change.paused, Archived: change.archived,
				}, &resp); err != nil {
					return nil, err
				}
				return resp.Item, nil
			})
		},
	}
}

var (
	backlogPauseCmd = backlogGroupCmd("pause", "Stop the runner from starting items in a parent",
		"Pause a parent: the runner and \"af backlog run\" skip its queued items, and the items "+
			"under it. Running items go on. \"af backlog start\" still starts an item by hand.",
		groupChange{paused: boolPtr(true)})
	backlogUnpauseCmd = backlogGroupCmd("unpause", "Let the runner start items in a paused parent",
		"Unpause a parent, so the runner can start its queued items again. An archived group "+
			"must be unarchived first.", groupChange{paused: boolPtr(false)})
	backlogArchiveCmd = backlogGroupCmd("archive", "Hide a group and its items from the list",
		"Archive a top-level parent: it and its items are hidden from \"af backlog list\" "+
			"unless --archived, and it is paused. Refused while an item in it is running, has an "+
			"open question, or has an answer on its way.", groupChange{archived: boolPtr(true)})
	backlogUnarchiveCmd = backlogGroupCmd("unarchive", "Show an archived group again, still paused",
		"Unarchive a group. It comes back paused; unpause it to let the runner start its items.",
		groupChange{archived: boolPtr(false)})
)

func init() {
	BacklogCmd.AddCommand(backlogPauseCmd, backlogUnpauseCmd, backlogArchiveCmd, backlogUnarchiveCmd)
}
