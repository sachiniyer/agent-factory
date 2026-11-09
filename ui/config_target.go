package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// WHICH daemon the `,` config editor administers (#3708).
//
// The TUI is launchable against a remote daemon (`af --daemon-url
// http://box:8443`, #1592 Phase 3 PR4) and `af config set` has routed its global
// write to that daemon since #3679. The config editor did not: it read this
// machine's config in-process and wrote this machine's control socket, so an
// operator whose whole session pointed at `box` opened `,`, saw their own
// laptop's values under their own path, edited one, and changed their own
// laptop. Nothing on screen said the target had been ignored.
//
// Both halves live in this one file because the answer is BOTH OR NEITHER, and
// that is a property worth making structural rather than conventional. Routing
// the write alone would be strictly worse than the bug: the rows the operator
// edits were read from machine A, so sending one to machine B writes a value
// that was never B's, over whatever B actually had. A form's read and its write
// are one feature. Splitting them across two files is how they come apart again.
//
// Three properties this file exists to hold:
//
//   - A remote target NEVER falls back to a local read or a local write. The
//     local path's dial fallback is right for the local socket (a config write
//     with no daemon running still has to work) and catastrophic for a remote
//     one, where it would silently answer about — or mutate — the operator's own
//     machine. So the remote branch has no fallback at all: every failure,
//     including "that daemon is too old to serve the route", is a refusal.
//   - The header names the daemon, then its OWN path, un-abbreviated. The pane
//     has never abbreviated $HOME to `~`, and it must not start: `~` is right
//     locally and a trap remotely, because whenever two of one operator's boxes
//     share a home layout the remote path collapses to
//     `~/.agent-factory/config.toml` and reads as the caller's own file. The
//     ORDER is the pane's own answer rather than the CLI's — see
//     remoteConfigLocation for the clipped header that decided it.
//   - Local behaviour is untouched. No target still means the in-process read
//     and daemon.SetGlobalConfigValue, error text included.
//
// The routing decision lives in ui/ rather than app/ because the pane's real
// write path already does (NewConfigPane wires applyingConfigSet), and moving it
// up would mean either a second writer or a save seam injected past the tests
// that drive the real one. app/ calls ReadConfigForEditor and stays a caller.

// ReadConfigForEditor reads the config the `,` overlay edits, from whichever
// daemon this af session is attached to. It returns the manifest rows zipped
// with live values, and the location label the pane puts in its header.
//
// repo is the active project (nil outside one). Its rows show the project's
// EFFECTIVE values with scope markers (#7009) — the edit still writes the global
// file. scope is the scope the rows were actually read in: repo when it was
// honored, nil when the read fell back to global values, so the pane's explain
// cannot answer for a different scope than its rows show.
//
// The app calls it (rather than the pane calling it itself) so the app keeps
// deciding WHEN to re-read: reopening the editor must show the file as it is
// now, including a hand-edit or an `af config set` made since the TUI started.
func ReadConfigForEditor(repo *config.RepoContext) (entries []config.ConfigEntry, location string, scope *config.RepoContext, err error) {
	if !apiclient.IsRemoteTarget() {
		return localConfigForEditor(repo)
	}
	client, err := apiclient.NewTargeted()
	if err != nil {
		return nil, "", nil, err
	}
	defer client.CloseIdleConnections()

	resp, err := client.GetConfig(daemon.GetConfigRequest{RepoPath: repo.WorkspacePath()})
	if err != nil {
		return nil, "", nil, remoteConfigRefusal(client, "read config from", "GetConfig", "Nothing was read", err)
	}
	// A real daemon always answers with the whole manifest — GetConfig takes no
	// key filter. But a REMOTE target is whatever answers the URL, and an empty
	// list would open an editor with no rows: a pane that silently offers nothing
	// to edit, on a machine the operator cannot see, is worse than a refusal that
	// says the URL may not name an af daemon.
	if len(resp.Entries) == 0 {
		return nil, "", nil, fmt.Errorf("the config editor read no config keys from the daemon at %s; "+
			"check that the URL names an af daemon", apiclient.RemoteTargetURL())
	}
	location = remoteConfigLocation(resp.Path)
	switch {
	case repo == nil:
		return resp.Entries, location, nil, nil
	case resp.ProjectRoot != "":
		return resp.Entries, location + " · project " + resp.ProjectRoot, repo, nil
	case resp.ProjectError != "":
		return resp.Entries, location + " · " + resp.ProjectError, nil, nil
	}
	// The daemon dropped repo_path without a word: it predates the field. Its
	// rows are global, and saying so is what keeps them from passing as the
	// project's.
	return resp.Entries, location + " · this daemon is too old to resolve project values — the rows are global", nil, nil
}

// rereadConfigForEditor is the pane's re-resolve seam: the same read the app
// opens the editor with, so a scoped row refreshed after a save is resolved
// exactly as the rows around it were.
func rereadConfigForEditor(repo *config.RepoContext) ([]config.ConfigEntry, *config.RepoContext, error) {
	entries, _, honored, err := ReadConfigForEditor(repo)
	return entries, honored, err
}

// localConfigForEditor is the in-process read, the local half of the target
// decision.
//
// A config that will not load is surfaced rather than swallowed: opening an
// editor onto a broken file and letting the user "fix" one key would write the
// rest of the broken state back. A project that will not resolve is not that:
// the global rows are still the file the editor writes, so it opens on them and
// the header says why the project values are missing — the same degrade the
// daemon answers a remote pane with.
func localConfigForEditor(repo *config.RepoContext) ([]config.ConfigEntry, string, *config.RepoContext, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, "", nil, fmt.Errorf("cannot open the config editor: %w", err)
	}
	configDir, err := config.GetConfigDir()
	if err != nil {
		return nil, "", nil, err
	}
	path := filepath.Join(configDir, config.TomlConfigFileName)
	if repo == nil {
		return config.ManifestWithValues(cfg), path, nil, nil
	}
	entries, root, err := config.EditorEntriesForRepo(repo, cfg)
	if err != nil {
		return config.ManifestWithValues(cfg), path + " · " + config.ProjectScopeError(repo.WorkspacePath(), err), nil, nil
	}
	return entries, path + " · project " + root, repo, nil
}

// ExplainConfigForEditor resolves one key's provenance on whichever daemon this
// af session is attached to — the explain half of the editor's read surface
// (#4803). The pane's rows say WHAT a value is; this says WHERE it came from,
// through the same resolver `af config get --explain` uses. repo is the scope the
// rows were read in (nil for global), so a project row is explained in its
// project.
//
// The split is the editor's own, for the editor's reason: a remote target never
// falls back to the local file, so the local branch resolves in-process (the
// same way localConfigForEditor reads its rows) while the remote branch asks the
// targeted daemon — explaining machine B from machine A's file would answer
// about a config the user is not looking at, the exact failure the read and the
// write already route together to prevent.
func ExplainConfigForEditor(key string, repo *config.RepoContext) (*config.ResolvedValue, error) {
	if !apiclient.IsRemoteTarget() {
		var value config.ResolvedValue
		var err error
		if repo == nil {
			value, err = config.ExplainGlobalValue(key)
		} else {
			value, err = config.ExplainRepoValue(repo, key)
		}
		if err != nil {
			return nil, err
		}
		return &value, nil
	}
	client, err := apiclient.NewTargeted()
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()

	resp, err := client.ExplainConfig(daemon.ExplainConfigRequest{Key: key, RepoPath: repo.WorkspacePath()})
	if err != nil {
		return nil, remoteConfigRefusal(client, "explain config on", "ExplainConfig", "Nothing was read", err)
	}
	// A real daemon answers with the key's trace or an error. A remote target is
	// whatever answers the URL, though, and an empty explanation would render a
	// view claiming every layer was absent — the fabricated-blank failure shape
	// this surface exists to avoid. Refuse it like the empty GetConfig list.
	if resp.Explanation.Key == "" {
		return nil, fmt.Errorf("the daemon at %s returned an empty config explanation; "+
			"check that the URL names an af daemon", apiclient.RemoteTargetURL())
	}
	return &resp.Explanation, nil
}

// applyingConfigSet writes one global config key on whichever daemon this
// session is attached to, and returns the per-key effect notice so the pane
// shows the honest outcome — live now, deferred rebind, next daemon start, or
// next af launch — computed by the write path itself rather than one canned
// sentence.
//
// It is the pane's injected save seam (NewConfigPane), so a test that swaps it
// is testing the pane's plumbing rather than inventing a second writer.
func applyingConfigSet(key, value string) (*config.SetResult, string, error) {
	if !apiclient.IsRemoteTarget() {
		return localConfigSet(key, value)
	}
	return remoteConfigSet(key, value)
}

// localConfigSet writes through a running LOCAL daemon's admission-gated
// SetConfigValue (#3231) — the same handler the web form posts — so a daemon
// that is quiescing or validating an upgrade refuses BEFORE the file changes,
// instead of the pane writing first and live-applying through an ungated poke.
// The daemon applies an accepted write to itself in place (#2480), so a TUI
// config edit still takes effect without a restart; with no daemon reachable the
// write lands locally, as before. Never spawns a daemon.
func localConfigSet(key, value string) (*config.SetResult, string, error) {
	resp, err := daemon.SetGlobalConfigValue(key, value)
	if err != nil {
		return nil, "", err
	}
	// resp.Warnings is the apply-time notice (the daemon's
	// ListenerExposureNotice) and is only populated when a daemon actually
	// applied the write. With no daemon reachable the apply fails and
	// resp.Warnings is nil, while resp.Result.Warnings still carries the
	// per-write exposureWarning config.SetGlobalConfigValue produced — the one
	// paneNotice's doc comment promises the pane surfaces. Use it only on that
	// fallback so a daemon-running edit shows exactly one notice, not the two
	// differently-worded ones both fields carry there.
	warnings := resp.Warnings
	if len(warnings) == 0 && resp.Result != nil {
		warnings = resp.Result.Warnings
	}
	return resp.Result, paneNotice(resp.RestartNotice, resp.ListenerAddr, warnings), nil
}

// remoteConfigSet writes the key on the targeted daemon over HTTP, through the
// same admission-gated handler the local socket reaches.
//
// The round trip runs on the bubbletea Update goroutine, where the local write
// already ran. That is not a regression introduced here: daemon.SetGlobalConfigValue
// dials the socket and calls rpc.Call with NO deadline at all, so a wedged local
// daemon blocks the TUI forever today, while this path is bounded by apiclient's
// remote dial and request timeouts. Making the save asynchronous would change the
// LOCAL pane's behaviour (the echo would arrive a frame later), so it belongs in
// a change that does both halves deliberately, not in this one.
func remoteConfigSet(key, value string) (*config.SetResult, string, error) {
	client, err := apiclient.NewTargeted()
	if err != nil {
		return nil, "", err
	}
	defer client.CloseIdleConnections()

	// The flat alias is the version-skew wire spelling, exactly as on the local
	// socket (daemon.SetGlobalConfigValue) and in `af config set --daemon-url`
	// (commands/configremote.go): an older daemon's allowlist predates the grouped
	// TOML name, a newer one canonicalizes the alias before writing, so send the
	// legacy spelling and canonicalize the echo back. A REMOTE daemon is the case
	// this matters most for — it is upgraded on its own schedule, and nothing
	// about the operator's own binary says which version answered.
	resp, err := client.SetConfigValue(daemon.SetConfigValueRequest{
		Key:   config.LegacyConfigKey(key),
		Value: value,
	})
	if err != nil {
		return nil, "", remoteConfigRefusal(client, "change config on", "SetConfigValue", "Nothing was written", err)
	}
	// A success envelope with no result in it. A real daemon cannot produce one —
	// its handler returns an error or fills Result — but a remote target is
	// whatever answers the URL, and commitEdit dereferences Result to echo the key
	// and value. A sentence naming the host beats a nil-pointer panic that takes
	// the TUI down with it.
	if resp.Result == nil {
		return nil, "", fmt.Errorf("the daemon at %s reported the config change succeeded but returned no result; "+
			"check that the URL names an af daemon", apiclient.RemoteTargetURL())
	}
	resp.Result.Key = config.CanonicalConfigKey(resp.Result.Key)
	// Same fallback as localConfigSet: resp.Warnings carries the apply-time
	// ListenerExposureNotice when the daemon applied the write, and
	// resp.Result.Warnings carries the per-write exposureWarning it did not. A
	// daemon that actually applied is the remote case, so this is dormant today,
	// but it makes the contract explicit — one notice, from whichever field the
	// daemon populated — rather than relying on the remote daemon always applying.
	warnings := resp.Warnings
	if len(warnings) == 0 {
		warnings = resp.Result.Warnings
	}
	return resp.Result, paneNotice(resp.RestartNotice, resp.ListenerAddr, warnings), nil
}

// paneNotice builds the one line the pane shows after a write: the daemon's
// per-key effect notice, then where that daemon is accepting if the key moved a
// listener (#3722), then any warnings — the tokenless-network exposure notice, or
// a listener rebind's actionable reason — so a TUI edit that exposes the API or
// could not rebind still tells the user.
//
// The address is folded in HERE, in the one place both write paths already meet,
// rather than at each of them. That is not tidiness: the local and remote save
// paths having to remember the same thing separately is exactly how the four
// config surfaces came to disagree about deferred rebinds (#3397).
//
// It matters most on the path this file added. Since the editor routes to the
// targeted daemon (#3741), a `,` edit of network.listen_addr against a
// --daemon-url session moves the very listener that session is talking over — so
// the pane is no longer merely informing the operator where the daemon went, it
// is telling them where their own next request has to go. The address is the
// daemon's own answer; nothing here derives it from the value that was typed,
// which would name a dead address whenever the rebind failed and the daemon
// stayed put.
func paneNotice(notice, listenerAddr string, warnings []string) string {
	if listenerAddr != "" {
		notice = notice + " Daemon now listening at " + listenerAddr + "."
	}
	if len(warnings) > 0 {
		notice = notice + " " + strings.Join(warnings, " ")
	}
	return notice
}

// applyingHostsEdit applies ONE per-entry edit of the host table (add or
// remove) on whichever daemon this session is attached to — the hosts_edit arm
// of the same SetConfigValue write path the pane's scalar saves already use
// (#5855). It is the save seam behind the pane's Hosts section (#5571 W10.5),
// injected like applyingConfigSet so a test that swaps it drives the pane's
// plumbing rather than a second writer.
//
// The local half is daemon.EditGlobalHosts — the same client `af hosts
// add|remove` rides, which Pings the running daemon for the ConfigHostsEdit bit
// itself before sending. The remote half performs that Ping-before-send itself
// (the health read is FRESH, not the section's cached arm state — exactly the
// gate the request type's comment prescribes and the web's hostsHealthNow
// runs): a daemon that predates SetConfigValueRequest.HostsEdit tolerates the
// unknown field and writes an EMPTY hosts table in its place, so the bit must
// be re-read at send time and an absent or unreadable bit refuses.
func applyingHostsEdit(edit config.HostsEdit) (*config.SetResult, string, error) {
	if !apiclient.IsRemoteTarget() {
		return localHostsEdit(edit)
	}
	return remoteHostsEdit(edit)
}

// localHostsEdit is the pane's local arm of applyingHostsEdit: the running
// daemon's own admission-gated hosts_edit when it answers, the file-locked
// local edit when it does not — the fallback is inside daemon.EditGlobalHosts,
// the same place `af hosts` gets it.
func localHostsEdit(edit config.HostsEdit) (*config.SetResult, string, error) {
	resp, err := daemon.EditGlobalHosts(edit)
	if err != nil {
		return nil, "", err
	}
	// Same warning selection as localConfigSet: the apply-time notices win when
	// the daemon applied the write, the write's own warnings otherwise.
	warnings := resp.Warnings
	if len(warnings) == 0 && resp.Result != nil {
		warnings = resp.Result.Warnings
	}
	return resp.Result, paneNotice(resp.RestartNotice, resp.ListenerAddr, warnings), nil
}

func remoteHostsEdit(edit config.HostsEdit) (*config.SetResult, string, error) {
	client, err := apiclient.NewTargeted()
	if err != nil {
		return nil, "", err
	}
	defer client.CloseIdleConnections()

	health, healthErr := client.Health(context.Background())
	if healthErr != nil || !health.ConfigHostsEdit {
		return nil, "", fmt.Errorf(
			"the daemon at %s (%s) cannot apply a host-list edit atomically; upgrade that daemon, then retry (nothing was written)",
			apiclient.RemoteTargetURL(), client.DaemonVersionPhrase(context.Background()))
	}
	resp, err := client.SetConfigValue(daemon.SetConfigValueRequest{
		Key:       "hosts",
		HostsEdit: &edit,
	})
	if err != nil {
		return nil, "", remoteConfigRefusal(client, "change config on", "SetConfigValue", "Nothing was written", err)
	}
	// Same empty-result guard as remoteConfigSet: a remote target is whatever
	// answers the URL, and the pane echoes fields of Result.
	if resp.Result == nil {
		return nil, "", fmt.Errorf("the daemon at %s reported the config change succeeded but returned no result; "+
			"check that the URL names an af daemon", apiclient.RemoteTargetURL())
	}
	warnings := resp.Warnings
	if len(warnings) == 0 {
		warnings = resp.Result.Warnings
	}
	return resp.Result, paneNotice(resp.RestartNotice, resp.ListenerAddr, warnings), nil
}

// CheckHostForEditor asks whichever daemon this session is attached to to probe
// a listed host — an empty name probes every listed host — and to record its
// readiness, the check half of the pane's Hosts section. The probe runs where
// the host list lives and the ssh would be composed: the local control socket
// for a local session, the targeted daemon's /v1/CheckHost under
// --daemon-url/AF_DAEMON_URL — never this machine's daemon for a remote
// target, which is the whole doctrine of this file.
//
// It is a dial, not a read, so the app runs it on an async command rather than
// the Update goroutine (contrast with the synchronous config writes, which are
// already bounded socket/HTTP calls).
func CheckHostForEditor(name string) (daemon.CheckHostResponse, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.CheckHost(daemon.CheckHostRequest{Host: name})
	}
	client, err := apiclient.NewTargeted()
	if err != nil {
		return daemon.CheckHostResponse{}, err
	}
	defer client.CloseIdleConnections()

	resp, err := client.CheckHost(daemon.CheckHostRequest{Host: name})
	if err != nil {
		if apiclient.IsRouteNotServed(err) {
			return daemon.CheckHostResponse{}, fmt.Errorf(
				"the daemon at %s (%s) does not serve the CheckHost route; the host was not probed — af never probes from this machine for a remote target. "+
					"Upgrade the daemon, or run `af hosts check` on the daemon host",
				apiclient.RemoteTargetURL(), client.DaemonVersionPhrase(context.Background()))
		}
		return daemon.CheckHostResponse{}, err
	}
	return resp, nil
}

// HostCheckOrigin is the "from X" phrase a probe verdict names — where the
// probe ran from the user's seat. The pane renders daemon.HostCheckLines with
// it: "the daemon host" under a remote target (the ssh is composed and dialed
// over there), "this machine" locally, matching `af hosts check`'s copy.
func HostCheckOrigin() string {
	if apiclient.IsRemoteTarget() {
		return "the daemon host"
	}
	return "this machine"
}

// remoteConfigLocation renders the pane header's label for a remote target: the
// daemon, then the daemon's own path verbatim.
//
// The URL comes FIRST, which is where this deliberately diverges from the
// `<path> on <url>` sentence `af config set --daemon-url` prints
// (commands/configremote.go configWriteLocation). That line is one unclipped
// terminal row; this one is not. The header is clipped rather than wrapped
// (#3430) at the width of the OVERLAY, not the terminal — measured at 72 columns
// inside a 120-column terminal — and with the path leading, a real label came out
// as:
//
//	Config  /home/dev/sandbox/remote-home/config.toml on http://127.0.0…
//
// losing the port, and on a longer path the host too. #3430 settled that a
// clipped path still says which file is open, and that was true while there was
// only one machine it could be on. Which MACHINE outranks which file here, so
// the truncation has to fall on the path.
//
// Not $HOME-abbreviated, whichever order: `~/.agent-factory/config.toml` is what
// the remote path collapses to whenever two of one operator's boxes share a home
// layout, and it then reads as the caller's own file.
func remoteConfigLocation(path string) string {
	url := apiclient.RemoteTargetURL()
	if path == "" {
		return url
	}
	return fmt.Sprintf("%s · %s", url, path)
}

// remoteConfigRefusal passes a remote failure through untouched, except for the
// one failure whose raw form is unactionable: a daemon that does not serve the
// route at all.
//
// apiclient classifies that positively rather than guessing — rpcHandler answers
// 200/400/405/413/500/503, so a 404 on a /v1 route can only come from the mux
// catch-all, which means the handler never ran (#3679). The distinction is the
// whole point: an envelope error is the daemon's considered answer and is final,
// while a missing route means nothing happened over there — and the tempting
// repair for THAT, "fall back to the local path like the socket does", would
// quietly answer about, or write, the operator's own machine.
func remoteConfigRefusal(client *apiclient.Client, action, route, nothing string, err error) error {
	if !apiclient.IsRouteNotServed(err) {
		return err
	}
	return fmt.Errorf(
		"the config editor cannot %s the daemon at %s: that daemon (%s) does not serve the %s route. "+
			"%s — af never falls back to this machine's config for a remote target. "+
			"Upgrade the daemon, or edit config on the daemon host",
		action, apiclient.RemoteTargetURL(), client.DaemonVersionPhrase(context.Background()), route, nothing)
}
