package commands

import (
	"os"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"

	"github.com/spf13/cobra"
)

// `af agent-server` (#1592 Phase 4 PR1) runs a headless, single-workspace
// agent-server over the HTTP/WS+TLS+token protocol — the process that will later
// run INSIDE each docker/ssh sandbox and be driven by a remote daemon over an
// authed URL. It is the standalone, out-of-process form of the daemon's
// in-process local agent-server: one session's workspace (worktree + tmux),
// exposed over the exact wire the daemon already speaks, behind a bearer token on
// a TLS listener.
//
// It is DARK in this PR: nothing provisions a sandbox to run it and nothing in the
// daemon drives it yet. Run it by hand and drive it directly to prove the process
// boundary.

var (
	agentServerListen  string
	agentServerRepo    string
	agentServerTitle   string
	agentServerProgram string
	agentServerAutoYes bool
)

var agentServerCmd = &cobra.Command{
	Use:   "agent-server",
	Short: "Run a headless single-workspace agent-server over HTTP/WS+TLS+token",
	Long: `Run a headless agent-server for exactly one session's workspace, served over
the same REST + WebSocket protocol the daemon speaks, behind a TLS listener that
requires a bearer token on every request.

This is the process that will later run inside a docker container or on an ssh
remote (#1592 Phase 4): a remote daemon dials the authed URL it exposes and
drives the workspace exactly as it drives a local in-process session. Run it
directly to reach one workspace over the network.

The listener is always TLS + token (the token must never ride the wire in the
clear). On startup it prints one JSON line to stdout carrying the bound address,
the bearer token, and the self-signed cert path/fingerprint to pin. On
SIGINT/SIGTERM it tears the workspace down (kills tmux, removes the worktree) —
durability of in-progress work is the driving daemon's job (push the branch
before shutdown), not this server's.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo := agentServerRepo
		if repo == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			repo = cwd
		}

		return daemon.RunAgentServer(daemon.AgentServerOptions{
			ListenAddr: agentServerListen,
			RepoPath:   repo,
			Title:      agentServerTitle,
			Program:    agentServerProgram,
			AutoYes:    agentServerAutoYes,
		}, cmd.OutOrStdout())
	},
}

func init() {
	agentServerCmd.Flags().StringVar(&agentServerListen, "listen", "127.0.0.1:0",
		"TLS TCP bind address (host:port); :0 lets the kernel pick a free port")
	agentServerCmd.Flags().StringVar(&agentServerRepo, "repo", "",
		"Repository path the workspace runs against (default: current directory)")
	agentServerCmd.Flags().StringVar(&agentServerTitle, "title", "",
		"Session title for the workspace (required)")
	agentServerCmd.Flags().StringVar(&agentServerProgram, "program", "",
		"Agent program to run (default: the configured default_program)")
	agentServerCmd.Flags().BoolVar(&agentServerAutoYes, "auto-yes", false,
		"Enable the agent-server's AutoYes accept for the workspace")
	_ = agentServerCmd.MarkFlagRequired("title")
}
