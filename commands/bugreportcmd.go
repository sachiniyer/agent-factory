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
	bugReportFile   bool
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

By default the redacted bundle is written to a single text file
(~/af-bug-report-<ts>.txt) and a pre-filled GitHub issue DRAFT is opened in your
browser. The draft ALWAYS targets the agent-factory project
(github.com/sachiniyer/agent-factory) — never whatever repo you happen to be
in — because it reports a bug in af itself.

The draft body carries a bounded, redacted summary of the key diagnostics
(versions, daemon status, counts, and the newest log lines) so the report is
useful even as filed; the complete bundle is too large for a URL, so it stays on
disk for you to attach. The draft is never submitted for you: review it, drag the
bundle file onto the issue, and click Submit yourself. If neither gh nor a
browser opener is available, the command falls back to just writing the bundle
file and printing where it is so you can attach it to an issue by hand.

Use -o/--output <path> or --file to skip GitHub and only write the bundle file.

REDACTION IS BEST-EFFORT. Free-text and secret-bearing fields (session titles,
session prompts, task prompts, tab commands, remote metadata) are dropped; $HOME and your
username are collapsed to ~ / [user]; and known credential shapes are scrubbed
wherever they appear. Perfect redaction is impossible — open the file and
review it before sharing it publicly.

Use --json to emit the structured manifest (wrapped in the shared {data,error}
envelope) to stdout instead of writing a file or opening a draft.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		info := collectDaemonStatus()
		var daemonHuman bytes.Buffer
		humanCmd := &cobra.Command{}
		humanCmd.SetOut(&daemonHuman)
		printDaemonStatusHuman(humanCmd, info)

		// Resolve the output path BEFORE building. The issue draft has to fit a
		// URL length cap, and it names this path — so the builder must measure
		// the real path into the body it size-checks. Substituting it afterwards
		// made the body that actually reached GitHub longer than the one that was
		// checked. Resolving first costs nothing: the path depends on flags and
		// the clock, never on the bundle.
		outPath, err := resolveBugReportPath(bugReportOutput)
		if err != nil {
			return err
		}

		result, err := bugreport.Build(bugreport.Inputs{
			AFVersion:    version,
			GeneratedAt:  time.Now().Format("2006-01-02 15:04:05 -0700"),
			DaemonStatus: info,
			DaemonHuman:  daemonHuman.String(),
			BundlePath:   outPath,
		})
		if err != nil {
			return err
		}

		if bugReportJSON {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(json.RawMessage(result.JSON)))
		}

		// The redacted bundle is always written to disk — in the default flow it
		// is the file the user drags onto the GitHub draft; in file-only mode it
		// is the whole output.
		if err := os.WriteFile(outPath, []byte(result.Text), 0600); err != nil {
			return fmt.Errorf("write bug report: %w", err)
		}

		w := cmd.OutOrStdout()

		// File-only: an explicit --output path or --file skips GitHub entirely.
		if bugReportFile || bugReportOutput != "" {
			fmt.Fprintf(w, "Wrote bug report to %s\n", outPath)
			fmt.Fprintln(w, "Attach this file to your bug report.")
			fmt.Fprintln(w, "Review it first — redaction is best-effort and cannot catch everything.")
			return nil
		}

		// Default: open a pre-filled GitHub issue DRAFT against the agent-factory
		// project repo — never the user's own repo, wherever they ran `af` from.
		// The body carries a bounded, redacted diagnostics excerpt; the complete
		// bundle is attached by the user. Nothing is submitted automatically.
		//
		// result.Body is used verbatim: it is already redacted (including the
		// bundle path it names, so the public draft can't leak $HOME/username)
		// and already size-checked. stdout still prints the real local path.
		opened, reason := openGitHubIssueDraft(result.Title, result.Body)
		if !opened {
			fmt.Fprintf(w, "Couldn't open a GitHub issue draft (%s).\n", reason)
			fmt.Fprintf(w, "Wrote the redacted bug-report bundle to %s\n", outPath)
			fmt.Fprintln(w, "Attach it to a new issue manually. Review it first — redaction is best-effort.")
			return nil
		}
		fmt.Fprintf(w, "Wrote the redacted bug-report bundle to %s\n", outPath)
		fmt.Fprintln(w, "Opened a pre-filled GitHub issue draft in your browser — it is NOT submitted.")
		fmt.Fprintln(w, "Attach the bundle file above to the draft (drag-and-drop), review both, then click Submit.")
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
		"Emit the structured manifest to stdout (in the {data,error} envelope) instead of writing a file or opening a draft")
	bugReportCmd.Flags().StringVarP(&bugReportOutput, "output", "o", "",
		"Write the bundle to this path and skip opening a GitHub draft (implies --file)")
	bugReportCmd.Flags().BoolVar(&bugReportFile, "file", false,
		"Only write the bundle file (to ~/af-bug-report-<ts>.txt); do not open a GitHub issue draft")
	rootCmd.AddCommand(bugReportCmd)
}
