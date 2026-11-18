package commands

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/log"
)

// Local-only command guard (#3661).
//
// --daemon-url and --token are PERSISTENT root flags (root.go), so cobra
// advertises them under every subcommand — including the ones that never open a
// client and can only ever answer about the machine they run on. Accepting the
// target there and dropping it is the worst of the available behaviours: an
// operator who runs `af config validate --daemon-url http://box:8443` gets a
// confident verdict about their OWN laptop, with nothing in the output saying
// the target was ignored, and `af config migrate` the same way lets them believe
// they rewrote the remote host's file. Within one command group the same flag is
// then honoured by one verb and ignored by four, which is how the misreading
// survives being noticed.
//
// So refuse, and refuse through one seam. requireLocalTarget is what a
// local-only subcommand added later calls, so it inherits the refusal by writing
// one line rather than by remembering a rule that is nowhere written down.
//
// apiclient.IsRemoteTarget is the same resolver the real client uses (flag > env,
// empty ⇒ the local unix socket), which is why the AF_DAEMON_URL spelling is
// covered without a second lookup here: someone who exported it did so because
// they mean the remote box, and a local-only verb answering about this machine
// is the same misreading whichever route the URL took.
//
// `af accounts` and `af quota` refuse on this same apiclient seam with their own
// wording (#2983, #3057). They predate this helper and their messages are pinned
// by their own tests, so they are left alone here rather than reworded; the seam
// they share is the part that matters.
//
// name is passed rather than derived from cobra's CommandPath because RunE is
// reached both through the real tree and, throughout this package's tests,
// through a bare &cobra.Command{} that has no path to print. A refusal whose
// wording depends on how it was invoked is not one worth pinning.
//
// does completes "<name> is local-only: it <does> and cannot …", so write it as
// a verb phrase naming the local thing the command touches.
func requireLocalTarget(name, does string) error {
	if !apiclient.IsRemoteTarget() {
		return nil
	}
	return fmt.Errorf("%s is local-only: it %s and cannot act on %s; "+
		"unset --daemon-url/AF_DAEMON_URL (and any saved target: af target clear) to act on this host, "+
		"or run %s on the daemon host",
		name, does, targetLabel(), name)
}

// targetLabel names the daemon a refusal is about, so the message says WHICH
// machine the verb declined to act on (W9.4). The URL comes from the same
// resolver the real client uses, so a flag, the env and a saved target all
// print what would actually have been dialled; the generic fallback covers a
// target that is configured but unreadable.
func targetLabel() string {
	if url := apiclient.RemoteTargetURL(); url != "" {
		return "the remote daemon at " + url
	}
	return "the configured remote daemon"
}

// requireLocalDaemonStart is the `af --daemon` variant of the local-only guard
// (#7558): `af --daemon` starts the daemon process on THIS machine, and no
// daemon API stands in for it. It refuses ONLY a remote target named
// explicitly on this invocation — the --daemon-url flag or AF_DAEMON_URL —
// because that is an operator standing here saying which daemon they mean;
// silently serving a local daemon anyway is the wrong-machine answer W9.4
// exists to prevent.
//
// A SAVED target never refuses `af --daemon`: the daemon is always local and
// a saved target is a client preference, not a directive typed at this
// process, so the start logs one INFO line naming it and proceeds. That split
// is what keeps every supervisor-owned launch working in a home that happens
// to hold a saved target — the autostart unit's `ExecStart=<path> --daemon`
// (daemon/autostart.go), rollback's startPreviousDaemon and the committed
// handoff's startArmedDaemonUnderOwner (daemon/upgrade_recovery.go,
// daemon/upgrade_forward.go) all spawn `af --daemon` with neither the flag
// nor the variable, so they meet no refusal either way.
//
// Source is read through ResolveTargetInfo — the same resolver `af target
// show` reports — rather than by re-reading the flag and env separately, so
// "explicit" here means exactly what the client means by flag-or-env.
func requireLocalDaemonStart() error {
	info := apiclient.ResolveTargetInfo()
	switch info.Source {
	case apiclient.TargetFlag, apiclient.TargetEnv:
		return fmt.Errorf("af --daemon is local-only: it starts the daemon process on this machine "+
			"and cannot act on the remote daemon at %s; "+
			"unset --daemon-url/AF_DAEMON_URL to start this host's daemon, "+
			"or run af --daemon on the daemon host", info.URL)
	case apiclient.TargetSaved:
		if info.Err != nil {
			// A saved target that fails closed still fails CLIENTS closed,
			// but a daemon start has no wrong host to act on — warn and
			// proceed so a corrupt client-target.json cannot wedge the
			// supervisor's restart of a daemon it never aimed at.
			log.WarningLog.Printf("af --daemon ignores the saved remote target, which failed to load: %v", info.Err)
		} else if info.URL != "" {
			log.InfoLog.Printf("af --daemon ignores saved remote target %s: a saved target is a client preference; the daemon it starts is always this machine's", info.URL)
		}
	}
	return nil
}
