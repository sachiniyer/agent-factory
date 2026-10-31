package session

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pushcred"
	"github.com/sachiniyer/agent-factory/log"
)

// Push credentials on listed hosts (#5571 W7.8): the host wiring.
//
// When the operator opted the session's host in (ProvisionSpec.PushCredentials),
// provisioning teaches the workspace to ask the daemon for its origin's GitHub
// credential instead of carrying one:
//
//   - the workspace's REPO-LOCAL git config (never ~/.gitconfig, W2.7) resets the
//     helper list for the origin's host to empty and then names af's own helper,
//     `<session>/af internal git-credential --session-dir <session>`, with
//     useHttpPath on so git says which repository it wants. The empty reset is
//     load-bearing: without it git also offers the credential to the host user's
//     global helpers, and a global `credential.helper=store` writes it to
//     ~/.git-credentials in plain text (measured on git 2.43);
//   - a gh shim, `<session>/gh`, first on the pane PATH, that runs the host's real
//     gh with the token in its environment alone;
//   - push-origin.json, the non-secret record of the origin the shim asks for.
//
// None of the three holds a secret. The token is fetched over the session's own
// scoped af callback each time git or gh asks, and the daemon answers only for
// this session's own origin, only while it is live and licensed. Removal is
// therefore the daemon's: every way a session ends revokes its callback. The
// files go with the session directory at kill, and a restore of a preserved
// workspace re-reconciles them with the licence as it stands then.

// pushOriginFor is the origin the wiring is for: the clone URL the daemon read
// from its own checkout, when it is an https GitHub remote the daemon would answer
// for (the same predicate the route applies).
func pushOriginFor(spec ProvisionSpec) (pushcred.Origin, bool) {
	return pushcred.ParseOrigin(spec.CloneURL, os.Getenv("GH_HOST"))
}

// listedHostPushCredentials reports whether the operator's global host list opts
// hostName into push credentials. A repository cannot set or extend the list, so
// a repo-chosen [ssh] target (an empty hostName) never qualifies.
func listedHostPushCredentials(cfg *config.ResolvedConfig, hostName string) bool {
	if cfg == nil || hostName == "" {
		return false
	}
	h, ok := cfg.Hosts[hostName]
	return ok && h.PushCredentials
}

// PushOriginPath and PushShimPath are the two non-secret files the wiring writes
// into the session directory.
func (w *sandboxWorkspace) PushOriginPath() string { return w.SessionDir + "/" + pushcred.OriginFile }
func (w *sandboxWorkspace) PushShimPath() string   { return w.SessionDir + "/" + pushcred.ShimFile }

// pushHelperValue is the helper git runs: af's own staged binary, named by
// absolute path (so no PATH entry chooses it), told which session it serves.
func (w *sandboxWorkspace) pushHelperValue() string {
	return "!" + shellQuote(w.AfPath()) + " internal git-credential --session-dir " + shellQuote(w.SessionDir)
}

// pushShimScript is the gh shim. `exec`, so the real gh's exit status is the
// shim's.
func (w *sandboxWorkspace) pushShimScript() string {
	return "#!/bin/sh\nexec " + shellQuote(w.AfPath()) + " internal gh-shim --session-dir " + shellQuote(w.SessionDir) + " -- \"$@\"\n"
}

// unsetAll removes every value of a repo-local key; a key that is absent (git's
// exit 5) is not an error.
func unsetAll(ws, key string) string {
	return "{ git -C " + ws + " config --local --unset-all " + shellQuote(key) + "; rc=$?; [ $rc -eq 0 ] || [ $rc -eq 5 ]; }"
}

// reconcilePushCredentials makes the workspace's wiring match the licence. On a
// fresh workspace with no licence it does nothing, so a session on a host that
// did not opt in is provisioned exactly as before. On an adopted (restored)
// workspace it always reconciles: a workspace wired under an earlier licence is
// unwired when the licence is gone.
//
// A failure never fails the provision. It degrades to a session without push
// credentials (git then fails to push exactly as it did before W7.8), and the
// reason goes to the daemon log: a session that cannot push is still far more
// useful than no session (the W3.1 rule for the callback itself).
func (w *sandboxWorkspace) reconcilePushCredentials(timeout time.Duration, adopted bool) {
	origin, ok := pushOriginFor(w.spec)
	wire := ok && w.spec.PushCredentials
	if !wire && !adopted {
		return
	}
	var err error
	if wire {
		err = w.wirePushCredentials(timeout, origin)
	} else {
		err = w.unwirePushCredentials(timeout)
	}
	if err != nil {
		log.WarningLog.Printf("session %q: push credentials are not wired (git push and gh will use only the host's own logins): %v", w.spec.Title, err)
	}
}

func (w *sandboxWorkspace) wirePushCredentials(timeout time.Duration, origin pushcred.Origin) error {
	ws := shellQuote(w.WorkspacePath())
	helperKey := "credential.https://" + origin.Host + ".helper"
	pathKey := "credential.https://" + origin.Host + ".useHttpPath"
	script := strings.Join([]string{
		unsetAll(ws, helperKey),
		"git -C " + ws + " config --local --add " + shellQuote(helperKey) + " ''",
		"git -C " + ws + " config --local --add " + shellQuote(helperKey) + " " + shellQuote(w.pushHelperValue()),
		"git -C " + ws + " config --local " + shellQuote(pathKey) + " true",
	}, " && ")
	if out, err := w.shell.Run(timeout, script, nil, true); err != nil {
		return fmt.Errorf("configuring the workspace's credential helper failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	record, err := json.Marshal(origin)
	if err != nil {
		return err
	}
	if out, err := w.shell.Run(timeout, "umask 077 && cat > "+shellQuote(w.PushOriginPath()), strings.NewReader(string(record)+"\n"), true); err != nil {
		return fmt.Errorf("writing the push origin record failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	shim := shellQuote(w.PushShimPath())
	if out, err := w.shell.Run(timeout, "umask 077 && cat > "+shim+" && chmod 700 "+shim, strings.NewReader(w.pushShimScript()), true); err != nil {
		return fmt.Errorf("writing the gh shim failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// unwirePushCredentials removes af's helper, useHttpPath, the shim and the origin
// record, and restores the value-free clone helper a fresh workspace has, so an
// unlicensed restore looks exactly like an unlicensed create.
func (w *sandboxWorkspace) unwirePushCredentials(timeout time.Duration) error {
	ws := shellQuote(w.WorkspacePath())
	steps := []string{"rm -f " + shellQuote(w.PushShimPath()) + " " + shellQuote(w.PushOriginPath())}
	if origin, ok := pushOriginFor(w.spec); ok {
		steps = append(steps,
			unsetAll(ws, "credential.https://"+origin.Host+".helper"),
			unsetAll(ws, "credential.https://"+origin.Host+".useHttpPath"))
		if restore := gitPersistCredentialCommand(w.spec.CloneURL, w.WorkspacePath()); restore != "" {
			steps = append(steps, restore)
		}
	}
	if out, err := w.shell.Run(timeout, strings.Join(steps, " && "), nil, true); err != nil {
		return fmt.Errorf("removing push credential wiring failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
