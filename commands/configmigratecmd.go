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

It changes spelling, never meaning. A value written on one line is carried over
exactly as its own bytes, quoting and all; a value spread over several lines (an
array, typically) is re-encoded compactly rather than relocated as raw text, so
its formatting can change even though its contents do not. Either way the
rewritten file is re-parsed before anything is saved, and a rewrite that would
change even one effective value is refused rather than written. The
readers of the old spellings stay, so an older config keeps loading and nothing
about your running configuration changes.

One thing to know before you DOWNGRADE. The grouped spellings have only been
read since 2026-08-14 (#3354); an af older than that does not know them and
falls back to the built-in default for a migrated key. For most keys that
default is the conservative one (strict host-key checking, no credential
mount). Two cases are not, both because the listener defaults to a LIVE 127.0.0.1:8443:
migrating network.require_token = true loses the token an older binary could
read (network.require_loopback_token only matters alongside it, being inert on
its own), and migrating an empty network.listen_addr hides the fact that the web
server was turned OFF, so an older binary starts one. Migrate compares what such
a binary saw before and after, and says so explicitly when a migration costs you
either. Restore the backup before such a downgrade.

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
	if result.ConvertedFromJSON {
		// This ran before any key migration and moved the original aside, so it
		// is reported whether or not a deprecated key was found.
		fmt.Fprintf(w, "converted the legacy config.json to %s · af kept the original beside it\n\n", path)
	}
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
		// The summary goes ABOVE the diff, not below it. A moved key renders as a
		// removed line and an added one, and the surrounding lines can shift with
		// it, so the raw diff of a two-key migration can look far larger than the
		// change it describes. A reader who meets that first is alarmed and then
		// reassured; this way they are told what it means before they read it.
		// Name the backup that was actually written. availableBackupPath numbers
		// the copy when config.toml.bak already exists, so a hardcoded
		// "config.toml.bak" here would point a reader recovering from a bad
		// migration at an OLDER file than the one this run just saved.
		fmt.Fprintf(w, "\nthe effective configuration is unchanged — af reads both spellings · moved keys show below as a removed and an added line, and %s holds the original\n", prettyPath(result.Backup))
		fmt.Fprintf(w, "\n%s\n", strings.TrimRight(result.Diff, "\n"))
	}
	for _, caution := range result.Cautions {
		fmt.Fprintf(w, "\ncaution — %s\n", caution)
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
