package commands

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/apiclient"
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
	return fmt.Errorf("%s is local-only: it %s and cannot act on a remote daemon; "+
		"unset --daemon-url/AF_DAEMON_URL to act on this host, or run %s on the daemon host",
		name, does, name)
}
