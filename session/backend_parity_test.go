package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEveryRegisteredRuntimeDeclaresOffBox is the mechanism behind the
// declaration, not a restatement of it.
//
// #2778 was a guard that enumerated the backends it knew about and missed a
// path. Any hand-written `kind == A || kind == B || kind == C` is that hazard
// waiting for the next backend: the new kind gets a Runtime from the registry
// and is silently absent from the condition. Here, a kind registered without a
// declaration fails this test instead.
func TestEveryRegisteredRuntimeDeclaresOffBox(t *testing.T) {
	for kind := range runtimeRegistry {
		_, declared := backendProvisionsOffBox[kind]
		assert.True(t, declared,
			"backend %q is registered as a runtime but does not declare whether it provisions off-box; "+
				"add it to backendProvisionsOffBox so create knows whether to resolve the repo's origin URL", kind)
	}
	for kind := range backendProvisionsOffBox {
		_, registered := runtimeRegistry[kind]
		assert.True(t, registered,
			"backend %q declares off-box-ness but has no registered runtime; the declaration is dead", kind)
	}
}

// The declaration must also cover every backend the config layer accepts, or a
// user could select one that create then treats as local.
func TestEverySupportedBackendDeclaresOffBox(t *testing.T) {
	for _, name := range config.SupportedBackends {
		kind, err := ParseBackendKind(name)
		require.NoError(t, err, "config lists %q as supported but it does not parse as a backend kind", name)
		_, declared := backendProvisionsOffBox[kind]
		assert.True(t, declared, "supported backend %q does not declare whether it provisions off-box", name)
	}
}

// The property itself: local uses the worktree in place, every off-box runtime
// clones from the durable store and therefore needs the origin URL.
func TestProvisionsOffBoxMatchesRuntimeShape(t *testing.T) {
	assert.False(t, BackendLocal.ProvisionsOffBox(), "a local session runs in the worktree it already has")
	for _, kind := range []BackendKind{BackendDocker, BackendSSH, BackendHook} {
		assert.True(t, kind.ProvisionsOffBox(),
			"%s provisions a workspace off the local filesystem and clones from origin", kind)
	}
	assert.False(t, BackendKind("not-a-backend").ProvisionsOffBox(),
		"an unregistered kind must not be treated as off-box")
}
