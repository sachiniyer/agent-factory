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
// agent-factory homes, daemons running a binary no install owns, directories
// holding nothing but a dead daemon socket (#3845), and daemon problems.
// Read-only by default; --fix applies only the remediations whose ancestry is
// verified (killing marked orphans of dead sessions, removing abandoned temp
// homes, killing daemons whose home was deleted or whose binary is a temp-dir
// build). Anything ambiguous is reported, never touched.
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
  - af daemons running a binary no install owns — one under the temp dir, or one
    whose file is gone from disk. A temp-dir binary is debris a test or debug run
    left behind; --fix stops it once it is old enough that no test run can still
    own it, and never when it serves this home or when which home it serves
    cannot be read. A missing binary is reported, not stopped: af upgrade
    replaces it in place, so every healthy daemon looks that way until it restarts
  - temp directories holding nothing but a daemon socket nobody answers on — the
    residue an abandoned daemon's bind left behind. --fix removes one with
    os.Remove rather than a recursive delete, so a directory that has gained
    anything since the scan fails instead of being swept up with it
  - daemon health: control socket, autostart unit, pid file, binary freshness
  - client/daemon version skew, and the ways a stale daemon survives an
    upgrade: a second daemon on this home, an autostart unit launching a
    different af binary than yours, several af installs at different versions,
    sockets left behind with no daemon answering, and an autostart unit that
    is installed but not actually supervising anything
  - remote-hook setup for the current repo: config completeness and
    launch_cmd/delete_cmd script presence/executability
    (skipped cleanly when no remote backend is configured)
  - pinned remote host-key directories under hook-hosts/ that no session owns.
    Hook names are one namespace for the whole machine, so this one spans every
    project rather than the current repo, and it runs whether or not this repo
    configures a remote backend. --fix removes a directory only on proof that
    no session owns it — live, archived, mid-kill and awaiting-teardown
    sessions all count — and removes nothing at all when any part of that
    inventory cannot be read

The version-skew check exists because a skewed daemon fails quietly: it keeps
answering while rejecting fields a newer client sends, which surfaces as
"unknown field <name>" and a hung UI rather than as an upgrade prompt.

Use --json to emit each check as {name, section, status, detail, remedy,
actionable} in the shared {data,error} envelope for scripting. Branch on
"actionable", not on the status or whether --fix supports the row. Actionable
means doctor established a specific unhealthy condition and named a correction
that must happen before the run is healthy. Some exact corrections are manual
and remain actionable even though --fix does not perform them.

UNKNOWN observations stay visible as advisory warnings with inspection
guidance, but they are not actionable: "inspect it and decide" is not a finding
that the run is unhealthy. A CI step or health probe should fail on the command
exit code (equivalently, JSON summary.unresolved > 0), which includes only
actionable rows.

A clean run and a run that did not finish looking are NOT the same thing, and
the exit code cannot tell them apart. A check that stops early — the temp-home
sweep hits a candidate budget on a machine with a very large temp dir — reports
no unhealthy condition for what it never looked at, so it exits 0 while having
assessed only part of the machine. Such a run says so: the summary line ends
with "INCOMPLETE" naming the checks that gave up, and summary.incomplete lists
them in --json. A probe that treats unresolved == 0 as healthy must require
summary.incomplete to be empty as well.

High-volume findings are summarized by default so the actionable problem is
visible first — process findings, abandoned temp homes, and dead-socket
directories, all of which run to hundreds or thousands on a busy machine. Use
--verbose to show each item behind those summaries.

Read-only by default. With --fix, applies the safe remediations — killing
orphans whose ancestry markers prove they came from a dead af session, removing
stale temp homes, stopping daemons proven to be running a temp-dir binary, and
removing directories holding nothing but a dead daemon socket — logging each
action. Ambiguous cases are always reported rather than acted on, and remain
advisory unless another check establishes a specific unhealthy condition.

Exits 1 when unresolved actionable issues remain, 0 when doctor established no
unhealthy condition (advisory warnings may still be present, and a check may
have stopped early — see summary.incomplete above).`,
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
		"apply safe remediations (kill verified orphans and leaked daemons, remove stale temp homes and dead-socket dirs)")
	doctorCmd.Flags().BoolVar(&doctorVerboseFlag, "verbose", false,
		"show per-process doctor findings instead of collapsed summaries")
	doctorCmd.Flags().BoolVar(&doctorJSONFlag, "json", false,
		"emit each check as JSON in the {data,error} envelope")
	rootCmd.AddCommand(doctorCmd)
}
