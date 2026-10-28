package commands

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
)

var targetSetTokenStdin bool

var targetCmd = &cobra.Command{
	Use:   "target",
	Short: "Point this client at a remote daemon once",
	Long: `Save, show or clear the remote daemon this machine's af talks to.

A saved target is the lowest-precedence source: --daemon-url / --token win, then
AF_DAEMON_URL / AF_DAEMON_TOKEN, then the saved target. It is stored in this
machine's af home with 0600 permissions. The saved token is sent only to the
saved URL, never to a host named by a flag or environment variable.

Run 'af target show' to see which target an invocation will use and why.`,
}

var targetSetCmd = &cobra.Command{
	Use:   "set <url>",
	Short: "Save a remote daemon target",
	Long: `Save <url> (http://, ws://, https:// or wss://) as this client's target.

Pass the bearer token on stdin with --token-stdin so it never appears in argv or
shell history:

    af token show | ssh box ...      # on the daemon host
    printf '%s' "$TOKEN" | af target set https://box.tailnet.ts.net --token-stdin

Without --token-stdin no token is saved (the daemon may be unauthenticated, or
you can still pass --token / AF_DAEMON_TOKEN per invocation).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		t := apiclient.SavedTarget{URL: strings.TrimSpace(args[0])}
		if targetSetTokenStdin {
			tok, err := readTokenLine(cmd.InOrStdin())
			if err != nil {
				return err
			}
			if tok == "" {
				return fmt.Errorf("--token-stdin was given but stdin held no token")
			}
			t.Token = tok
		}
		if err := apiclient.SaveTarget(t); err != nil {
			return err
		}
		path, _ := apiclient.SavedTargetPath()
		fmt.Fprintf(cmd.OutOrStdout(), "saved target %s (token %s) in %s\n", t.URL, setOrNot(t.Token != ""), path)
		return nil
	},
}

var targetShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show which daemon this invocation targets, and why",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		info := apiclient.ResolveTargetInfo()
		out := cmd.OutOrStdout()
		if info.Err != nil {
			return info.Err
		}
		if info.URL == "" {
			fmt.Fprintln(out, "target: local daemon (unix socket); source: local — no --daemon-url, AF_DAEMON_URL or saved target")
			return nil
		}
		fmt.Fprintf(out, "target: %s; source: %s; token: %s\n", info.URL, info.Source, setOrNot(info.HasToken))
		return nil
	},
}

var targetClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Forget the saved target",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := apiclient.ClearSavedTarget(); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "saved target cleared")
		return nil
	},
}

func setOrNot(b bool) string {
	if b {
		return "set"
	}
	return "not set"
}

func readTokenLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func init() {
	targetSetCmd.Flags().BoolVar(&targetSetTokenStdin, "token-stdin", false, "Read the bearer token from stdin (first line) and save it with the target")
	targetCmd.AddCommand(targetSetCmd, targetShowCmd, targetClearCmd)
}
