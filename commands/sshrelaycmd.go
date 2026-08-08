package commands

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/internal/sshrelay"
)

// sshRelayCmd is the ProxyCommand af composes into its own ssh invocations so
// every step of a remote session dials one resolved address (#3086). Hidden: it
// is af talking to af, never something an operator runs — the same convention as
// hook-guard-tmux.
//
// STDOUT IS THE SSH TRANSPORT STREAM while this runs, which shapes every choice
// below:
//
//   - The command's output writer is pointed at STDERR, so cobra itself can never
//     reach stdout — not a usage dump, not a help screen, not an error. Only the
//     explicit os.Stdout handed to sshrelay.Run carries bytes.
//   - SilenceUsage, so a malformed invocation prints one line rather than a usage
//     block. (The root command's PersistentPreRun sets this too; stating it here
//     keeps the guarantee local to the command that depends on it.)
//   - RunE does NO af startup work: no config load, no log.Initialize, no
//     ensureDaemonForTasks. The root command's PersistentPreRun only sets
//     SilenceUsage, and every other side effect lives in the bare-`af` RunE this
//     path never enters — so a relay cannot start a daemon or migrate a config.
//     TestSSHRelayWritesNothingButRelayedBytesToStdout runs the REAL binary and
//     asserts the byte stream, which is what makes that a checked property rather
//     than a claim about the call graph.
//
// The port is a separate argument from the address rather than one `host:port`
// string, so an IPv6 literal is bracketed by net.JoinHostPort inside the relay
// instead of by a caller that might forget.
var sshRelayCmd = &cobra.Command{
	Use:          sshrelay.Subcommand + " <address> <port>",
	Short:        "Internal: relay stdio to a pinned TCP address for af's own ssh ProxyCommand",
	Hidden:       true,
	Args:         cobra.ExactArgs(2),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return sshrelay.Run(args[0], args[1], cmd.InOrStdin(), os.Stdout)
	},
}

func init() {
	// Everything cobra prints for this command goes to stderr. OutOrStdout() is
	// what the help and usage writers consult, and its default is os.Stdout.
	sshRelayCmd.SetOut(os.Stderr)
	rootCmd.AddCommand(sshRelayCmd)
}
