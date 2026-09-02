package commands

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// `af config migrate` is the answer to a deprecated-key warning (#3624). Before
// it, every such warning ended in "no file was rewritten" and named no command
// that would rewrite it — a notice the reader could not act on, repeated on
// every config load, and inherited by every external user with an older config
// on upgrade. The warning now names this verb; this verb ends the warning.
//
// All of the judgment lives in config.MigrateGlobalConfig, which shares its
// deprecation table with the warnings themselves. This file is presentation.
var configMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Rewrite deprecated config keys to their current spelling",
	Long: `Rewrite the deprecated keys in the global config to their current spelling, in
place, and print the diff. This is the command the deprecated-key warnings name.

It changes spelling, never meaning. Each value is carried over exactly as it was
written, the rewritten file is re-parsed before anything is saved, and a rewrite
that would change even one effective value is refused rather than written. The
readers of the old spellings stay, so an older config keeps loading and nothing
about your running configuration changes.

One thing to know before you downgrade. The grouped spellings have been read
since 2026-08-14 (#3354). An af older than that does not know them, so it would
fall back to the built-in default for a migrated key rather than read the
value — for every migrated key that default is the conservative one (a loopback
listener, strict host-key checking, no credential mount), so the fallback is
safe rather than surprising. config.toml.bak holds the old spellings if you need
to go back further.

The previous file is kept beside it as config.toml.bak (an existing backup is
never overwritten; the copy is numbered instead). A legacy config.json is
converted to config.toml on the way in, exactly as any af start would convert it.

Running it twice is safe: the second run finds nothing to migrate.

Two things it will not do:

  A key written in BOTH spellings with different values is refused, naming the
  key. af has a documented winner at load time — the grouped value — but no
  migration should make that tie-break permanent on your behalf.

  root_agents is reported and left exactly where it is. Its successor is a
  registered project's personal [root_agent], so migrating it would mean
  registering projects for you: durable state outside this file. The keys that
  can move still move.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		result, err := config.MigrateGlobalConfig()
		if err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
		}
		if configJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(result))
		}
		writeMigrationReport(cmd.OutOrStdout(), result)
		return nil
	},
}

// writeMigrationReport renders a finished migration for a human: what moved,
// the exact bytes that changed, and what is still deprecated afterwards.
func writeMigrationReport(w io.Writer, result *config.MigrationResult) {
	path := prettyPath(result.Path)
	if !result.Changed() {
		fmt.Fprintf(w, "nothing to migrate in %s\n", path)
	} else {
		fmt.Fprintf(w, "migrated %s in %s · backup %s\n\n",
			pluralKeys(len(result.Migrated)), path, prettyPath(result.Backup))
		for _, migrated := range result.Migrated {
			if migrated.Redundant {
				fmt.Fprintf(w, "  %s → dropped · %s already carried the same value\n", migrated.From, migrated.To)
				continue
			}
			fmt.Fprintf(w, "  %s → %s\n", migrated.From, migrated.To)
		}
		fmt.Fprintf(w, "\n%s\n", strings.TrimRight(result.Diff, "\n"))
		fmt.Fprintln(w, "\nthe effective configuration is unchanged — af reads both spellings · config.toml.bak holds the old ones")
	}
	for _, left := range result.Left {
		fmt.Fprintf(w, "\nleft in place — %s has no in-file migration · %s\n", left.Key, left.Step)
		for _, detail := range left.Detail {
			fmt.Fprintf(w, "  %s\n", detail)
		}
	}
}

func pluralKeys(n int) string {
	if n == 1 {
		return "1 key"
	}
	return fmt.Sprintf("%d keys", n)
}

func init() {
	configMigrateCmd.Flags().BoolVar(&configJSONFlag, "json", false,
		"Emit the migration result wrapped in the {data,error} envelope")
	configCmd.AddCommand(configMigrateCmd)
}
