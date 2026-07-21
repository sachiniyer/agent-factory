package commands

import (
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"

	"github.com/spf13/cobra"
)

// `af agent-server` (#1592 Phase 4 PR1) runs a headless, single-workspace
// agent-server over the HTTP/WS+token protocol — the process that will later
// run INSIDE each docker/ssh sandbox and be driven by a remote daemon over an
// authed URL. It is the standalone, out-of-process form of the daemon's
// in-process local agent-server: one session's workspace (worktree + tmux),
// exposed over the exact wire the daemon already speaks, behind a bearer token on
// a plain-HTTP listener.
//
// It is DARK in this PR: nothing provisions a sandbox to run it and nothing in the
// daemon drives it yet. Run it by hand and drive it directly to prove the process
// boundary.

var (
	agentServerListen     string
	agentServerRepo       string
	agentServerTitle      string
	agentServerProgram    string
	agentServerSessionEnv []string
)

var agentServerCmd = &cobra.Command{
	Use:   "agent-server",
	Short: "Run a headless single-workspace backend (not the web UI — that is 'af daemon')",
	Long: `Run a headless agent-server for exactly one session's workspace, served over
the same REST + WebSocket protocol the daemon speaks, behind a plain-HTTP
listener that requires a bearer token on every request.

This does not start the web UI, and serves no frontend at all — opening its port
in a browser returns a 404 saying so. If you want the browser app, run the
daemon — any 'af' command starts it — and open http://localhost:8443. The web UI
is bundled into the daemon and served from its listen_addr; agent-server is only
the headless per-workspace backend that a daemon drives, and it exists to be
consumed by a daemon rather than opened by a person.

This is the process that runs inside a docker container or on an ssh remote
(#1592 Phase 4): a remote daemon dials the authed URL it exposes and drives the
workspace exactly as it drives a local in-process session. Run it directly only
to host one workspace as a backend for a daemon on another machine.

The listener always requires the token and serves plain HTTP (no TLS) — reach it
over a private network or a tunnel (the docker/ssh runtimes forward a loopback
port). Its token is mandatory for every peer, whatever the global require_token
key says: that key governs only the daemon's own web listener. On startup
it prints one JSON line to stdout carrying the bound address and the bearer
token. On SIGINT/SIGTERM it tears the workspace down (kills tmux, removes the
worktree) — durability of in-progress work is the driving daemon's job (push the
branch before shutdown), not this server's.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveAgentServerRepo(agentServerRepo)
		if err != nil {
			return err
		}

		return daemon.RunAgentServer(daemon.AgentServerOptions{
			ListenAddr:            agentServerListen,
			RepoPath:              repo,
			Title:                 agentServerTitle,
			Program:               agentServerProgram,
			SessionEnvPassthrough: agentServerSessionEnv,
		}, cmd.OutOrStdout())
	},
}

// resolveAgentServerRepo resolves the --repo flag to the absolute repository
// path the workspace runs against, defaulting to the current directory when the
// flag is unset. A "~/repo" the shell did not expand (single-quoted, or via a
// variable) would otherwise reach NewInstance verbatim and be absolutized into
// "<cwd>/~/repo": the server still binds and prints its token, and the corrupted
// path only surfaces later when the worktree is set up (#1842).
func resolveAgentServerRepo(repoFlag string) (string, error) {
	if repoFlag == "" {
		// os.Getwd is already absolute and cannot contain a leading "~".
		return os.Getwd()
	}
	abs, err := config.ResolveUserPath(repoFlag)
	if err != nil {
		return "", fmt.Errorf("failed to resolve --repo path %q: %w", repoFlag, err)
	}
	return abs, nil
}

func init() {
	agentServerCmd.SetFlagErrorFunc(removedAutoYesFlagError)
	agentServerCmd.Flags().StringVar(&agentServerListen, "listen", "127.0.0.1:0",
		"HTTP TCP bind address (host:port); :0 lets the kernel pick a free port")
	agentServerCmd.Flags().StringVar(&agentServerRepo, "repo", "",
		"Repository path the workspace runs against (default: current directory)")
	agentServerCmd.Flags().StringVar(&agentServerTitle, "title", "",
		"Session title for the workspace (required)")
	agentServerCmd.Flags().StringVar(&agentServerProgram, "program", "",
		"Agent program to run (default: the configured default_program)")
	agentServerCmd.Flags().StringSliceVar(&agentServerSessionEnv, "session-env", nil,
		"Additional exact environment variable name an agent may inherit (repeatable)")
	_ = agentServerCmd.MarkFlagRequired("title")
}
