package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/bugreport"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/spf13/cobra"
)

var (
	bugReportJSON   bool
	bugReportOutput string
)

// bugReportCmd is `af bug-report` (#1048): bundle the daemon log tail,
// versions, configured tasks, the redacted session state, the daemon health
// snapshot, and the redacted global config into ONE shareable, best-effort
// redacted file a user can attach to a bug report. It is read-only — it reads
// local state and writes a single output file, never touching sessions, the
// daemon, or the network.
//
// A single concatenated text file (not a tarball) is deliberate: the bundle
// carries only text, and the "review before sharing" contract is only real if
// the user can open one file and read the whole thing in one scroll before
// attaching it. A tarball would hide the contents behind an extract step and
// invite blind, un-reviewed sharing of a redaction miss.
var bugReportCmd = &cobra.Command{
	Use:   "bug-report",
	Short: "Bundle logs, versions, tasks, and redacted state for a bug report",
	Long: `Collect a single, shareable diagnostics bundle for triage:

  - the daemon log tail (bounded to the last ~2MiB / 5000 lines)
  - versions: af, Go, OS/arch, and the daemon's version snapshot
  - the configured tasks (redacted)
  - the session state from instances.json (redacted)
  - the daemon health snapshot (same no-spawn probe as af daemon status)
  - the global config file, if any (redacted)

Everything is written to a single text file (default ~/af-bug-report-<ts>.txt)
so you can read the whole thing in one scroll before attaching it.

REDACTION IS BEST-EFFORT. Free-text and secret-bearing fields (session titles,
task prompts, tab commands, remote metadata) are dropped; $HOME and your
username are collapsed to ~ / [user]; and known credential shapes are scrubbed
wherever they appear. Perfect redaction is impossible — open the file and
review it before sharing it publicly.

Use --json to emit the structured manifest (wrapped in the shared {data,error}
envelope) to stdout instead of writing a file.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		info := collectDaemonStatus()
		var daemonHuman bytes.Buffer
		humanCmd := &cobra.Command{}
		humanCmd.SetOut(&daemonHuman)
		printDaemonStatusHuman(humanCmd, info)

		result, err := bugreport.Build(bugreport.Inputs{
			AFVersion:    version,
			GeneratedAt:  time.Now().Format("2006-01-02 15:04:05 -0700"),
			DaemonStatus: info,
			DaemonHuman:  daemonHuman.String(),
		})
		if err != nil {
			return err
		}

		if bugReportJSON {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(json.RawMessage(result.JSON)))
		}

		outPath, err := resolveBugReportPath(bugReportOutput)
		if err != nil {
			return err
		}
		if err := os.WriteFile(outPath, []byte(result.Text), 0600); err != nil {
			return fmt.Errorf("write bug report: %w", err)
		}

		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "Wrote bug report to %s\n", outPath)
		fmt.Fprintln(w, "Attach this file to your bug report.")
		fmt.Fprintln(w, "Review it first — redaction is best-effort and cannot catch everything.")
		return nil
	},
}

// resolveBugReportPath returns the output path: the explicit --output when
// given, else a timestamped file in the user's home directory, falling back to
// the current directory when the home directory cannot be resolved. The file
// is written 0600 because, even redacted, it may contain residual sensitive
// context until the user reviews it.
func resolveBugReportPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	name := fmt.Sprintf("af-bug-report-%s.txt", time.Now().Format("20060102-150405"))
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return name, nil
	}
	return filepath.Join(home, name), nil
}

func init() {
	bugReportCmd.Flags().BoolVar(&bugReportJSON, "json", false,
		"Emit the structured manifest to stdout (in the {data,error} envelope) instead of writing a file")
	bugReportCmd.Flags().StringVarP(&bugReportOutput, "output", "o", "",
		"Write the bundle to this path instead of the default ~/af-bug-report-<ts>.txt")
	rootCmd.AddCommand(bugReportCmd)
}
