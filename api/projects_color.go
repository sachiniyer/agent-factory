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

The web client uses this color as its accent, fills the top bar with it, and
paints the browser tab's favicon with it, so you can find the project's tab or
window quickly.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// An id goes to the daemon verbatim, as for `rebind`: the daemon reads only
		// that record, so an unrelated broken one cannot block the change.
		id := args[0]
		if config.ValidateProjectID(id) != nil {
			project, err := config.ResolveProjectSelector(id)
			if err != nil {
				return jsonError(err)
			}
			id = project.ID
		}
		updated, err := setProjectColorViaDaemon(daemon.SetProjectColorRequest{ID: id, Color: args[1]})
		if err != nil {
			return jsonError(err)
		}
		return jsonOut(updated)
	},
}
