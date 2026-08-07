package daemon

import (
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// The PHYSICAL sandbox reap, observable at the daemon layer (#3042).
//
// registerStartedRemote supplies an agent-server endpoint and no teardown, so
// remoteAgentServer.Kill posts /v1/agent/kill and skips the reap callback
// entirely. Every test built on it could therefore observe only the REST MESSAGE:
// a regression that sent the kill and left the container running kept the counter
// at one and passed.
//
// That is worse than an ordinary fixture gap because of what it hides. A sandbox
// left alive is a VM or container still billing, and af keeps no record of a
// runtime it believes it destroyed — so nothing will ever come back for it. The
// reap is also the irreversible half of archive, which is the decision
// #2923/#2925/#2959 are about, so the suite covering that decision could not see
// the one act it cannot undo.
//
// The rule these fixtures follow: assert the runtime is GONE, through the same
// field a docker/ssh runtime populates to reap it (ProvisionResult.Teardown).
// Not a log line, not a returned string, not an absence of error. A better proxy
// is still a proxy.

// sandboxReap is a runtime's reap callback that records what happened to it.
//
// It stands where ProvisionResult.Teardown stands, which is the point: docker's is
// `docker rm -f`, ssh's is the remote-dir cleanup plus tunnel close, and both reach
// the instance through this one field. A test that counts calls here is counting
// the same event production performs, one indirection from the container itself.
type sandboxReap struct {
	mu    sync.Mutex
	calls int
	err   error
	// witness is sampled on EVERY reap, before the count is bumped, so a test can
	// assert what had already happened when the runtime was released — the push, in
	// practice. Ordering asserted this way is physical: it reads the reap's own
	// vantage point rather than inferring order from two independent counters that
	// both end at one whatever sequence produced them.
	witness   func() int32
	witnessed []int32
}

// fail makes the reap report err. Used for the unknown-state and completed-error
// arms, which are different outcomes rather than degrees of the same one.
func (r *sandboxReap) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// observe installs the sampler described on witness.
func (r *sandboxReap) observe(w func() int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.witness = w
}

func (r *sandboxReap) run() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.witness != nil {
		r.witnessed = append(r.witnessed, r.witness())
	}
	r.calls++
	return r.err
}

// count is how many times the sandbox was physically released.
func (r *sandboxReap) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// witnessedAtFirstReap reports what the sampler saw when the runtime was first
// released, or -1 if it was never released.
func (r *sandboxReap) witnessedAtFirstReap() int32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.witnessed) == 0 {
		return -1
	}
	return r.witnessed[0]
}

// registerStartedRemoteWithReap is registerStartedRemote plus an observable
// physical reap.
//
// A VARIANT rather than a change to registerStartedRemote in place, deliberately.
// Thirty-odd call sites share that helper and most of them are status-poll tests
// where no reap should ever happen; giving them all a teardown would change what
// they exercise, and silently — the poll paths would begin running a callback they
// never ran before. So the reap is opt-in, and a test that opts in is a test whose
// subject is the reap.
//
// The teardown is installed AFTER the instance is registered, which is safe only
// because SetRuntimeTeardownForTest invalidates the derived agentSrv cache
// (#1729): remoteAgentServer captures teardown by value at build time, so a reap
// installed against a warm cache would never run and this fixture would report
// zero reaps no matter what production did.
func registerStartedRemoteWithReap(t *testing.T, m *Manager, repoID, repoPath, title, url string, status session.Status) (*session.Instance, *remoteWorkspaceBackend, *sandboxReap) {
	t.Helper()
	inst, backend := registerStartedRemote(t, m, repoID, repoPath, title, url, status)
	reap := &sandboxReap{}
	session.SetRuntimeTeardownForTest(inst, reap.run)
	return inst, backend, reap
}
