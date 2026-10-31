package session

import (
	"os"

	"github.com/sachiniyer/agent-factory/internal/pushcred"
)

// Push credentials on listed hosts (#5571 W7.8): the two session facts the
// daemon's licence reads before it answers a session's request for its origin's
// GitHub credential. Both come from the DAEMON's own records — the runtime handle
// the provision persisted and the daemon checkout's origin — never from anything
// the session's host reports, which is the point: the host is the party asking.

// PushPlacement is where an off-box session's workspace runs, as the push
// credential licence reads it.
type PushPlacement struct {
	// ListedHost is the global host-list name an ssh session was placed on,
	// whether it was picked or came from default_host. Empty for every other
	// placement, a repository's own [ssh] target included.
	ListedHost string
	// FreeFormSandbox is true for a session on the operator's free-form
	// sandbox.ssh target.
	FreeFormSandbox bool
}

// PushCredentialPlacement reports where this session's workspace runs, from the
// runtime handle its provision recorded (and a reattach restored). ok is false
// for a session with no off-box runtime af can name: local, docker, hook, a
// repository-chosen [ssh] target, or a runtime that has been reaped.
func (i *Instance) PushCredentialPlacement() (PushPlacement, bool) {
	provider, ok := i.currentBackend().(runtimeCleanupProvider)
	if !ok {
		return PushPlacement{}, false
	}
	data := provider.runtimeCleanupData()
	switch {
	case data == nil:
		return PushPlacement{}, false
	case data.SSH != nil && data.SSH.HostName != "":
		return PushPlacement{ListedHost: data.SSH.HostName}, true
	case data.Sandbox != nil:
		return PushPlacement{FreeFormSandbox: true}, true
	}
	return PushPlacement{}, false
}

// PushCredentialOrigin is the origin a push credential may be answered for: the
// daemon checkout's own `origin`, read now, when it is an HTTPS GitHub remote
// (github.com or the daemon's GH_HOST). ok is false for anything else, an ssh
// origin included — that one authenticates with the host's own keys.
func (i *Instance) PushCredentialOrigin() (pushcred.Origin, bool) {
	if i == nil || i.Path == "" {
		return pushcred.Origin{}, false
	}
	return pushcred.ParseOrigin(originRemoteURL(i.Path), os.Getenv("GH_HOST"))
}
