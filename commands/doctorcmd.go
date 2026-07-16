package commands

import (
	"os"

	"github.com/sachiniyer/agent-factory/doctor"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/spf13/cobra"
)

var doctorFixFlag bool
var doctorSetupFlag bool
var doctorVerboseFlag bool

// doctorJSONFlag switches `af doctor` from the human report to the shared
// {data,error} envelope, matching `af config`/`af token`'s --json.
var doctorJSONFlag bool

// doctorCmd is `af doctor` (#1044, #1104): detect orphaned session
// processes, runaway CPU children, leaked af_ tmux sessions, stale temp
// agent-factory homes, and daemon problems. Read-only by default; --fix
// applies only the remediations whose ancestry is verified (killing marked
// orphans of dead sessions, removing abandoned temp homes, killing daemons
// whose home was deleted). Anything ambiguous is reported, never touched.
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose setup, daemon health, and leaked session resources",
	Long: `Diagnose the local agent-factory environment.

For first-run setup checks, use:

  af doctor --setup

The setup profile checks the prerequisites needed to create the first local
session: AF home writability, config materialization and parsing, git and the
current repo, git identity, tmux, configured agent commands, state/log storage,
daemon health, and remote-hook setup when this repo configures it.

Without --setup, doctor runs the full maintenance sweep for problems that
accumulate silently on a machine running agent-factory:

  - orphaned processes spawned by sessions that no longer exist
  - processes that escaped a live session's pane, or peg a CPU core for hours
  - af_ tmux sessions with no backing session record
  - abandoned agent-factory homes under the temp dir (leaked by tests/debug runs)
  - daemon health: control socket, autostart unit, pid file, binary freshness
  - client/daemon version skew, and the ways a stale daemon survives an
    upgrade: a second daemon on this home, an autostart unit launching a
    different af binary than yours, several af installs at different versions,
    sockets left behind with no daemon answering, and an autostart unit that
    is installed but not actually supervising anything
  - remote-hook setup for the current repo: config completeness, hook-script
    presence/executability, and a bounded list_cmd connectivity probe
    (skipped cleanly when no remote backend is configured)

The version-skew check exists because a skewed daemon fails quietly: it keeps
answering while rejecting fields a newer client sends, which surfaces as
"unknown field <name>" and a hung UI rather than as an upgrade prompt.

Use --json to emit each check as {name, section, status, detail, remedy,
actionable} in the shared {data,error} envelope for scripting. Branch on
"actionable", not on the status: doctor emits advisory warnings (no autostart
unit, a legacy config that still loads) that carry a remedy while leaving the
run healthy. Only the actionable rows make it exit nonzero.

High-volume process findings are summarized by default so the actionable
problem is visible first. Use --verbose to show each process behind those
summaries.

Read-only by default. With --fix, applies the safe remediations — killing
orphans whose ancestry markers prove they came from a dead af session, and
removing stale temp homes — logging each action. Ambiguous cases are always
reported rather than acted on.

Exits 1 when unresolved issues remain, 0 when healthy.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		report, err := doctor.Run(doctor.Options{Fix: doctorFixFlag, Setup: doctorSetupFlag, Version: version})
		if err != nil {
			return jsonWrapError(cmd, doctorJSONFlag, err)
		}
		if doctorJSONFlag {
			if err := doctor.RenderJSON(cmd.OutOrStdout(), report, doctorFixFlag, doctorVerboseFlag); err != nil {
				return err
			}
		} else {
			doctor.Render(os.Stdout, report, doctorFixFlag, doctorVerboseFlag)
		}
		if report.UnresolvedCount() > 0 {
			// Distinguish "problems found" from cobra usage errors without
			// printing a redundant error line.
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Exit(1)
		}
		return nil
	},
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorSetupFlag, "setup", false,
		"run the first-run setup profile (prerequisites, config, agent commands)")
	doctorCmd.Flags().BoolVar(&doctorFixFlag, "fix", false,
		"apply safe remediations (kill verified orphans, remove stale temp homes)")
	doctorCmd.Flags().BoolVar(&doctorVerboseFlag, "verbose", false,
		"show per-process doctor findings instead of collapsed summaries")
	doctorCmd.Flags().BoolVar(&doctorJSONFlag, "json", false,
		"emit each check as JSON in the {data,error} envelope")
	rootCmd.AddCommand(doctorCmd)
}
