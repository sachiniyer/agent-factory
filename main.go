package main

import (
	"os"

	"github.com/sachiniyer/agent-factory/commands"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session"
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
)

var (
	// version is the dev-build fallback. Released binaries are stamped at
	// build time via -ldflags "-X main.version=..." (see .github/workflows);
	// stable releases also commit the new number here so dev builds report
	// the latest stable base. Preview releases (vX.Y.Z-preview-N, #1041)
	// never rewrite this value.
	version     = "1.0.293"
	rootCommand = commands.NewRootCommand
)

func main() {
	// Installed BEFORE HandleInternalExec, which is the only consumer: the pane
	// shim resolves an account name against this machine's AF home, and
	// internal/agentaccount cannot be reached from internal/sessionenv because the
	// dependency already runs the other way (#3051).
	sessionenv.AccountLookup = func(agent, name string) (sessionenv.Account, error) {
		home, err := config.GetConfigDir()
		if err != nil {
			return sessionenv.Account{}, err
		}
		return agentaccount.Selected(home, agent, name)
	}
	// The exec shim cross-checks the env-supplied launch proof against what the
	// launcher would have produced for this pane's resolved operator config;
	// the env var is writable by the same shell that re-invokes af under the
	// marker, so the derivation is what an overwriting shell cannot forge
	// (#3123, #4731 review).
	sessionenv.AccountLaunchProofResolver = session.ResolveAccountLaunchProof
	sessionenv.HandleInternalExec()
	sessiontmux.HandleDedicatedServerExec()
	// Consume the internal __upgrade-recovery invocation (the persistent recovery
	// job execs the preserved previous binary this way) before Cobra, exactly as
	// the session exec protocol above. An ordinary invocation returns immediately.
	daemon.HandleUpgradeRecoveryExec()
	rootCmd := rootCommand(commands.Options{Version: version})
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
