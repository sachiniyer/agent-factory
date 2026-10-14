package daemon

import (
	"errors"
	"net/rpc"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for #4822's mixed-version rule on the socket transport — the same
// fail-closed gate the HTTP client applies (apiclient/rebind_guarded_test.go
// covers that twin). A guarded send goes to RebindProjectGuarded so the method
// itself is the capability check (#4888 review): a daemon that predates the
// method answers net/rpc's "can't find method", which is definitive proof the
// precondition would be silently dropped — the refusal lands instead of a
// last-writer-wins apply. No Ping probe decides, because a probe is never
// affine with the mutation it predicts.

func TestRebindProjectGuardedRefusesOnAPreCapabilityDaemon(t *testing.T) {
	var methodSent string
	old := callRebindProjectRPC
	callRebindProjectRPC = func(method string, _ RebindProjectRequest, _ *RebindProjectResponse) error {
		methodSent = method
		// net/rpc's verbatim answer when the method set has no such method.
		return rpc.ServerError("rpc: can't find method Control.RebindProjectGuarded")
	}
	t.Cleanup(func() { callRebindProjectRPC = old })

	_, err := RebindProject(RebindProjectRequest{
		ID:                 "prj_x",
		Path:               "/n",
		ExpectedRoot:       "/r",
		ExpectedCheckoutID: "chk-observed",
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrGuardedRebindUnsupported),
		"a daemon that lacks the guarded method must refuse the send, got %v", err)
	assert.Contains(t, err.Error(), "daemon host")
	assert.Equal(t, "RebindProjectGuarded", methodSent,
		"a guarded request must reach the guarded method — that is the capability check")
}

func TestRebindProjectGuardedKeepsOtherTransportErrorsActionable(t *testing.T) {
	old := callRebindProjectRPC
	callRebindProjectRPC = func(_ string, _ RebindProjectRequest, _ *RebindProjectResponse) error {
		return errors.New("socket unreadable")
	}
	t.Cleanup(func() { callRebindProjectRPC = old })

	_, err := RebindProject(RebindProjectRequest{
		ID:                 "prj_x",
		Path:               "/n",
		ExpectedRoot:       "/r",
		ExpectedCheckoutID: "chk-observed",
	})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrGuardedRebindUnsupported),
		"only 'can't find method' maps to the sentinel — an unrelated transport error stays itself, got %v", err)
}

func TestRebindProjectUnguardedUsesThePlainMethod(t *testing.T) {
	var methodSent string
	old := callRebindProjectRPC
	callRebindProjectRPC = func(method string, _ RebindProjectRequest, _ *RebindProjectResponse) error {
		methodSent = method
		return errors.New("no daemon in sandbox")
	}
	t.Cleanup(func() { callRebindProjectRPC = old })

	_, err := RebindProject(RebindProjectRequest{ID: "prj_x", Path: "/n"})
	require.Error(t, err, "no daemon answers in the test sandbox")
	assert.Equal(t, "RebindProject", methodSent,
		"an unguarded request keeps last-writer-wins on every daemon vintage")
	assert.False(t, errors.Is(err, ErrGuardedRebindUnsupported))
}
