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
// The app calls it (rather than the pane calling it itself) so the app keeps
// deciding WHEN to re-read: reopening the editor must show the file as it is
// now, including a hand-edit or an `af config set` made since the TUI started.
func ReadConfigForEditor() ([]config.ConfigEntry, string, error) {
	if !apiclient.IsRemoteTarget() {
		return localConfigForEditor()
	}
	client, err := apiclient.NewTargeted()
	if err != nil {
		return nil, "", err
	}
	defer client.CloseIdleConnections()

	resp, err := client.GetConfig(daemon.GetConfigRequest{})
	if err != nil {
		return nil, "", remoteConfigRefusal(client, "read config from", "GetConfig", "Nothing was read", err)
	}
	// A real daemon always answers with the whole manifest — GetConfig takes no
	// key filter. But a REMOTE target is whatever answers the URL, and an empty
	// list would open an editor with no rows: a pane that silently offers nothing
	// to edit, on a machine the operator cannot see, is worse than a refusal that
	// says the URL may not name an af daemon.
	if len(resp.Entries) == 0 {
		return nil, "", fmt.Errorf("the config editor read no config keys from the daemon at %s; "+
			"check that the URL names an af daemon", apiclient.RemoteTargetURL())
	}
	return resp.Entries, remoteConfigLocation(resp.Path), nil
}

// localConfigForEditor is the unchanged local read, moved here verbatim from
// app.showConfigEditor so both halves of the target decision sit together.
//
// A config that will not load is surfaced rather than swallowed: opening an
// editor onto a broken file and letting the user "fix" one key would write the
// rest of the broken state back.
func localConfigForEditor() ([]config.ConfigEntry, string, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, "", fmt.Errorf("cannot open the config editor: %w", err)
	}
	configDir, err := config.GetConfigDir()
	if err != nil {
		return nil, "", err
	}
	return config.ManifestWithValues(cfg), filepath.Join(configDir, config.TomlConfigFileName), nil
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
	return resp.Result, noticeWithWarnings(resp.RestartNotice, resp.Warnings), nil
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
	return resp.Result, noticeWithWarnings(resp.RestartNotice, resp.Warnings), nil
}

// noticeWithWarnings folds any warnings — the tokenless-network exposure notice,
// or a listener rebind's actionable reason — into the one line the pane shows,
// so a TUI edit that exposes the API or could not rebind still tells the user.
func noticeWithWarnings(notice string, warnings []string) string {
	if len(warnings) > 0 {
		notice = notice + " " + strings.Join(warnings, " ")
	}
	return notice
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
