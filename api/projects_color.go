package api

import (
	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// setProjectColorViaDaemon is the daemon seam, overridable in tests.
var setProjectColorViaDaemon = daemon.SetProjectColor

var projectsColorCmd = &cobra.Command{
	Use:   "color <project> <color>",
	Short: "Set the display color of a registered project",
	Long: `Set the display color of a registered project.

<project> is a prj_ id or a path inside the project's checkout. <color> is a hex
color such as '#2f5fd8' or '2f5fd8'. Pass "" to clear the color.

The web client paints the frame around the session and the browser tab's
favicon with this color, so you can find the project's tab or window quickly.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		project, err := config.ResolveProjectSelector(args[0])
		if err != nil {
			return jsonError(err)
		}
		updated, err := setProjectColorViaDaemon(daemon.SetProjectColorRequest{ID: project.ID, Color: args[1]})
		if err != nil {
			return jsonError(err)
		}
		return jsonOut(updated)
	},
}
