package commands

import (
	"github.com/spf13/cobra"
)

// Target classes (W9.4). --daemon-url / AF_DAEMON_URL / a saved target promises
// that a verb acts on THAT daemon. Every verb in the tree therefore falls into
// exactly one of these, and targetclass_test.go fails the build for a verb that
// is in none, so a new verb cannot silently answer about the wrong machine.
const (
	// classRouted: the verb follows the target — it reads or writes the target
	// daemon over the API, and never falls back to this machine.
	classRouted = "routed"
	// classRefusedCentral: the verb can only act on THIS machine (a file, a
	// process, a unit in its af home) and no daemon API stands in for it, so it
	// refuses a remote target before running, naming the daemon it declined to
	// act on. Enforced for all of them in one place, refuseLocalOnlyUnderRemoteTarget.
	classRefusedCentral = "refused"
	// classRefusedInVerb: refused too, but by the verb's own guard because its
	// wording or its --project/--flag shape needs the context only the verb has.
	classRefusedInVerb = "refused-in-verb"
	// classLocal: acts on the client by design — the binary, its terminal, its
	// own saved target, static help — so the target is irrelevant to it.
	classLocal = "local-by-design"
)

// localOnlyVerbs are the classRefusedCentral verbs and the verb phrase that
// completes "<verb> is local-only: it <does> and cannot act on the daemon at
// <url>". Each has no daemon API: the token file, the autostart unit, the
// daemon process and the af home are properties of the machine the verb runs on.
var localOnlyVerbs = map[string]string{
	"af token show":       "prints the bearer token stored in this machine's af home",
	"af token rotate":     "rewrites the bearer token file in this machine's af home",
	"af daemon restart":   "restarts the daemon process on this machine",
	"af daemon install":   "installs an autostart unit on this machine",
	"af daemon uninstall": "removes this machine's autostart unit",
	"af daemon adopt":     "takes over a daemon process on this machine",
	"af doctor":           "diagnoses this machine's daemon, config and tmux",
	"af reset":            "wipes this machine's af home",
}

// refuseLocalOnlyUnderRemoteTarget is the root PersistentPreRunE check: a
// classRefusedCentral verb under a remote target returns requireLocalTarget's
// refusal, which names the verb and the daemon it declined to act on.
func refuseLocalOnlyUnderRemoteTarget(cmd *cobra.Command) error {
	if does, ok := localOnlyVerbs[cmd.CommandPath()]; ok {
		err := requireLocalTarget(cmd.CommandPath(), does)
		// The hook runs before the verb's own handler, so the verb's --json error
		// contract has to be honoured here: with --json a refusal is the same
		// {data,error} envelope every other failure of the verb prints, not a
		// bare "Error:" line a script cannot parse (#5857).
		return jsonWrapError(cmd, jsonModeRequested(cmd), err)
	}
	return nil
}

// jsonModeRequested reports whether the verb was invoked with --json (or
// --json=true). It reads the parsed flag, so it works for every verb that
// declares one without a table of which do.
func jsonModeRequested(cmd *cobra.Command) bool {
	f := cmd.Flags().Lookup("json")
	return f != nil && f.Value.String() == "true"
}
