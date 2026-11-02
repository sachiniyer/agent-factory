package session

import (
	"os"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pushcred"
)

// Push credentials on listed hosts (#5571 W7.8): the session facts the daemon's
// licence reads before it answers a session's request for its origin's GitHub
// credential. All of them come from the DAEMON's own records — the runtime handle
// the provision persisted, the row's provenance, the daemon checkout's origin —
// never from anything the session's host reports, which is the point: the host
// is the party asking.

// PushPlacement is where an off-box session's workspace runs, as the push
// credential licence reads it.
type PushPlacement struct {
	// ListedHost is the global host-list name an ssh session was placed on,
	// whether it was picked or came from default_host. Empty for every other
	// placement, a repository's own [ssh] target included.
	ListedHost string
	// Destination is the ssh destination ListedHost resolved to at placement:
	// the machine the session actually runs on. A host-list entry re-pointed
	// since names a different machine, and its licence is not this session's.
	Destination config.SSHConfig
	// FreeFormSandbox is true for a session on the operator's free-form
	// sandbox.ssh target.
	FreeFormSandbox bool
	// SandboxSSH is the sandbox.ssh command a free-form session was placed with.
	SandboxSSH string
	// CallbackRef is the runtime's callback identity (#5571 W3.6): the ref its
	// af callback credential derives from, stable across daemon restarts and new
	// for a replacement runtime. Empty when no callback was granted.
	CallbackRef string
	// Machine is the machine the runtime runs on, as RuntimeMachine names it: what
	// the host-sharing rule compares against every other live session.
	Machine RuntimeMachine
}

// PushCredentialPlacement reports where this session's workspace runs, from the
// runtime handle its provision recorded (and a reattach restored). ok is false
// for a session with no off-box runtime af can name: local, docker, hook, a
// repository-chosen [ssh] target, or a runtime that has been reaped.
//
// Read under the instance lock: a teardown clears the runtime handle under it.
func (i *Instance) PushCredentialPlacement() (PushPlacement, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	provider, ok := i.backend.(runtimeCleanupProvider)
	if !ok {
		return PushPlacement{}, false
	}
	data := provider.runtimeCleanupData()
	switch {
	case data == nil:
		return PushPlacement{}, false
	case data.SSH != nil && data.SSH.HostName != "":
		return PushPlacement{ListedHost: data.SSH.HostName, Destination: data.SSH.Config, CallbackRef: data.SSH.CallbackRef, Machine: machineOf(data)}, true
	case data.Sandbox != nil:
		return PushPlacement{FreeFormSandbox: true, SandboxSSH: data.Sandbox.SSHCommand, CallbackRef: data.Sandbox.CallbackRef, Machine: machineOf(data)}, true
	}
	return PushPlacement{}, false
}

// Licensed reports whether the operator's live config licenses this placement:
// the placement agrees with the row's backend (a listed host is an ssh
// placement, the free-form target a sandbox one), the operator's opt-in is on,
// and the opted-in entry still names the machine the session runs on. A listed
// host re-pointed elsewhere, or a sandbox.ssh command changed, licenses the NEW
// target's sessions, not this one.
func (p PushPlacement) Licensed(cfg *config.Config, backend string) bool {
	if cfg == nil {
		return false
	}
	switch {
	case p.ListedHost != "" && backend == string(BackendSSH):
		entry, ok := cfg.Hosts[p.ListedHost]
		return ok && entry.PushCredentials && sameSSHDestination(sshConfigForHost(entry), p.Destination)
	case p.FreeFormSandbox && backend == string(BackendSandbox):
		recorded := strings.TrimSpace(p.SandboxSSH)
		return cfg.SandboxPushCredentials && recorded != "" && recorded == strings.TrimSpace(cfg.SandboxSSH)
	}
	return false
}

// sameSSHDestination reports whether two ssh configs reach the same login on
// the same machine: host, user and port. The identity file and host-key posture
// change how af connects, not where.
func sameSSHDestination(a, b config.SSHConfig) bool {
	port := func(p int) int {
		if p == 0 {
			return sshDefaultPort
		}
		return p
	}
	return a.Host != "" && strings.EqualFold(strings.TrimSpace(a.Host), strings.TrimSpace(b.Host)) &&
		a.User == b.User && port(a.Port) == port(b.Port)
}

// provenance is who created a session: InstanceData.CreatedBySandbox and
// CreatedByOperator. Immutable after construction, so read without a lock.
type provenance struct {
	createdBySandbox  string
	createdByOperator bool
}

// CreatedBySandbox is the id of the session whose sandbox callback credential
// created this one, empty for a session the operator created (#5571 W7.8).
func (i *Instance) CreatedBySandbox() string {
	return i.createdBySandbox
}

// OperatorCreated reports whether the row records that the operator's own
// authority created it. False for a sandbox-created row AND for one of unknown
// provenance (InstanceData.CreatedByOperator), which fails closed.
func (i *Instance) OperatorCreated() bool {
	return i.createdByOperator && i.createdBySandbox == ""
}

// ProvenanceText describes a row that OperatorCreated refuses, for a message
// that names it.
func (i *Instance) ProvenanceText() string {
	if i.createdBySandbox != "" {
		return "created by another session's af"
	}
	return "of unknown provenance (created before af recorded who creates a session)"
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

// carrierLockTries and carrierLockWait bound how long a carrier lookup waits for
// a provisioner whose tunnel lock is held. A reap holds it through its whole
// remote teardown, and a session being reaped carries nothing worth answering, so
// the lookup gives up rather than wait: refusing is the safe side.
const (
	carrierLockTries = 5
	carrierLockWait  = 2 * time.Millisecond
)

// CallbackCarrierRef reports the callback ref of the session whose supervised
// ssh tunnel is process group pgid, and whether one is. The daemon's sandbox
// endpoint learns the peer of each connection from the socket; every -R forward
// is delivered by the ssh client of ONE session's tunnel, which leads its own
// process group (spawnTunnel's Setpgid). So the group names the forward a
// request arrived through, and the ref names the credential that forward exists
// to carry — a credential presented through any other session's forward does not
// match (#5571 W7.8).
func CallbackCarrierRef(pgid int) (string, bool) {
	if pgid <= 0 {
		return "", false
	}
	var ref string
	var found bool
	liveSandboxTunnels.Range(func(key, _ any) bool {
		if r, ok := key.(*sandboxProvisioner).carriedCallbackRef(pgid); ok {
			ref, found = r, true
			return false
		}
		return true
	})
	return ref, found
}

// carriedCallbackRef reports this provisioner's callback ref when its live tunnel
// generation is process group pgid and carries a callback forward.
func (p *sandboxProvisioner) carriedCallbackRef(pgid int) (string, bool) {
	locked := false
	for try := 0; try < carrierLockTries; try++ {
		if p.reapMu.TryLock() {
			locked = true
			break
		}
		time.Sleep(carrierLockWait)
	}
	if !locked {
		return "", false
	}
	defer p.reapMu.Unlock()
	t := p.tunnel
	if p.stopped || t == nil || t.cmd == nil || t.cmd.Process == nil || len(p.reverseFwds) == 0 || p.callbackRef == "" {
		return "", false
	}
	select {
	case <-t.done:
		return "", false // this generation exited; its pid may be reused
	default:
	}
	if t.cmd.Process.Pid != pgid {
		return "", false
	}
	return p.callbackRef, true
}
