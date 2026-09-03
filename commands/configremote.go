package commands

import (
	"context"
	"fmt"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// Routing the GLOBAL `af config set`/`unset` write to the targeted daemon
// (#3679).
//
// #3678 made these two verbs refuse a remote target outright, because what they
// did before was worse than refusing: daemon.SetGlobalConfigValue dials
// DaemonSocketPath() — the LOCAL unix socket — and on a failed dial writes THIS
// machine's config.toml, so `af config set default_program codex --daemon-url
// http://box:8443` printed `set default_program = codex in
// ~/.agent-factory/config.toml`. A success line, naming a path on the wrong
// machine, for a mutation the operator believed they had made on another host.
//
// Refusing was honest but it shut a door that is genuinely open. The daemon has
// served an admission-gated SetConfigValue since #3231 — it is the write path the
// web config form posts to, and the one every first-class surface routes through
// while a daemon runs — so a remote client has something real to call. Routing to
// it is what makes --daemon-url mean ONE thing across the whole `af config`
// group instead of "honoured by nothing, ignored by everything".
//
// Three properties this file exists to hold, each of which was a way to get it
// wrong:
//
//   - A remote target NEVER falls back to a local write. The local path's dial
//     fallback is right for the local socket (a config write with no daemon
//     running still has to work) and catastrophic for a remote one: the fallback
//     would silently write the operator's own machine, which is exactly the
//     silent lie #3678 closed. So the remote branch has no fallback at all — every
//     failure, including "that daemon is too old to serve the route", is a
//     refusal.
//   - `--project` stays refused. It writes a registered project's machine-local
//     config, a personal override file that no remote daemon owns under any
//     routing, so it is local-only in the same sense the read verbs are.
//   - The routing decision lives HERE, above both layers, because neither can
//     host it: apiclient imports daemon, so daemon cannot ask apiclient which
//     target is configured, and apiclient's methods are thin parity twins of the
//     daemon's handlers rather than policy.

// globalConfigSet performs `af config set`'s global write against whichever
// daemon this invocation targets: the local one (unchanged — dial the control
// socket, fall back to a validated local write) or, when --daemon-url /
// AF_DAEMON_URL names a remote daemon, that daemon's SetConfigValue over HTTP.
//
// The response type is the same either way, so the command's printing, --json
// envelope, warnings and effect notice are one code path over both transports.
func globalConfigSet(key, value string) (daemon.SetConfigValueResponse, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.SetGlobalConfigValue(key, value)
	}
	client, err := apiclient.NewTargeted()
	if err != nil {
		return daemon.SetConfigValueResponse{}, err
	}
	defer client.CloseIdleConnections()

	// The flat alias is the version-skew wire spelling, exactly as on the local
	// socket (daemon.SetGlobalConfigValue): an older daemon's allowlist predates
	// the grouped TOML name, a newer one canonicalizes the alias before writing,
	// so send the legacy spelling and canonicalize the echo back. A REMOTE daemon
	// is the case this matters most for — it is upgraded on its own schedule, and
	// nothing about the operator's own binary says which version answered.
	resp, err := client.SetConfigValue(daemon.SetConfigValueRequest{
		Key:   config.LegacyConfigKey(key),
		Value: value,
	})
	if err != nil {
		return daemon.SetConfigValueResponse{}, remoteConfigWriteError(client, "af config set", "SetConfigValue", err)
	}
	if resp.Result == nil {
		return daemon.SetConfigValueResponse{}, missingRemoteResult("af config set")
	}
	resp.Result.Key = config.CanonicalConfigKey(resp.Result.Key)
	return resp, nil
}

// globalConfigUnset is globalConfigSet's counterpart for `af config unset`
// without --project: the local socket path unchanged, or the targeted daemon's
// UnsetConfigValue — a separate RPC from SetConfigValue, behind the same
// admission gate, answering in the same envelope.
//
// No alias normalization here, matching daemon.UnsetGlobalConfigValue: the unset
// key space is the three migrated backend settings, and the config package
// removes BOTH storage spellings of whichever alias it is given, so there is no
// skew spelling to choose between.
func globalConfigUnset(key string) (daemon.UnsetConfigValueResponse, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.UnsetGlobalConfigValue(key)
	}
	client, err := apiclient.NewTargeted()
	if err != nil {
		return daemon.UnsetConfigValueResponse{}, err
	}
	defer client.CloseIdleConnections()

	resp, err := client.UnsetConfigValue(daemon.UnsetConfigValueRequest{Key: key})
	if err != nil {
		return daemon.UnsetConfigValueResponse{}, remoteConfigWriteError(client, "af config unset", "UnsetConfigValue", err)
	}
	if resp.Result == nil {
		return daemon.UnsetConfigValueResponse{}, missingRemoteResult("af config unset")
	}
	return resp, nil
}

// missingRemoteResult covers a success envelope with no result in it. A real
// daemon cannot produce one — its handler returns an error or fills Result — but
// a REMOTE target is whatever answers the URL, and the two callers immediately
// dereference Result to print the key and path. Failing with a sentence that
// says which host answered beats a nil-pointer panic on the operator's terminal.
func missingRemoteResult(name string) error {
	return fmt.Errorf("%s: the daemon at %s reported success but returned no result; "+
		"check that the URL names an af daemon", name, apiclient.RemoteTargetURL())
}

// remoteConfigWriteError passes a remote write failure through untouched, except
// for the one failure whose raw form is unactionable: a daemon that does not
// serve the route at all.
//
// That case is not hypothetical for `unset` — the UnsetConfigValue route ships
// in this same change, so every daemon already deployed answers its 404 — and it
// is the case where doing the obvious thing would be worst. `daemon does not
// serve /v1/UnsetConfigValue (it answered 404: …)` names a path the operator
// never typed and says nothing about what changed, what did not, or what to do;
// and the tempting repair — "fall back to the local write, like the local socket
// does" — would put the change on the caller's own machine, which is the whole
// defect #3678 closed. So: name the daemon, name its version, state plainly that
// nothing was written, and give the two ways forward.
func remoteConfigWriteError(client *apiclient.Client, name, route string, err error) error {
	if !apiclient.IsRouteNotServed(err) {
		return err
	}
	return fmt.Errorf(
		"%s cannot change config on the daemon at %s: that daemon (%s) does not serve the %s route. "+
			"Nothing was written — af never falls back to writing this machine's config for a remote target. "+
			"Upgrade the daemon, or run %s on the daemon host",
		name, apiclient.RemoteTargetURL(), remoteDaemonVersion(client), route, name)
}

// remoteDaemonVersion asks the targeted daemon what version it is, for the
// refusal above. It runs only on that refusal path, never on a successful write,
// so the happy path stays one round trip.
//
// All three answers are stated as facts about the daemon rather than collapsed
// into "unknown". An empty Version from a RESPONDING daemon is itself positive
// evidence (see daemon.PingResponse.Version): the field rides Ping since #1044,
// so a daemon that omits it is older than that — which is consistent with, and
// further narrows, the missing route. A health probe that fails outright is a
// different fact again and is reported as one.
func remoteDaemonVersion(client *apiclient.Client) string {
	health, err := client.Health(context.Background())
	switch {
	case err != nil:
		return fmt.Sprintf("its version could not be read: %v", err)
	case health.Version == "":
		return "it reports no version, so it predates version reporting"
	default:
		return "version " + health.Version
	}
}

// configWriteLocation renders WHERE a config write landed, for the success line.
//
// prettyPath abbreviates $HOME to ~, which is right for a local write and a trap
// for a remote one: the daemon host's path is a path on ANOTHER machine, and
// `~/.agent-factory/config.toml` — which is what it collapses to whenever the two
// hosts share a home layout, the common case for one operator's own boxes —
// reads as the caller's own file. That is the sentence #3679 is about. So a
// remote write prints the daemon's path verbatim and names the daemon it
// belongs to.
func configWriteLocation(path string) string {
	if url := apiclient.RemoteTargetURL(); url != "" {
		return fmt.Sprintf("%s on %s", path, url)
	}
	return prettyPath(path)
}
