package integration_test

import (
	"errors"

	"github.com/sachiniyer/agent-factory/session"
)

// placementRecorder stands in for the daemon's credential object in suites that
// create sessions with no daemon. A session on a repository's own [ssh] target
// is a placement the operator didn't choose, and is provisioned only once a
// recorder takes it (#5571 W7.8: session.PlacementRecorder). It mints no
// callback, exactly as these suites ran before with no minter at all.
type placementRecorder struct{}

func (placementRecorder) Mint() (session.SandboxCredential, error) {
	return session.SandboxCredential{}, errors.New("this suite runs with no af callback")
}

func (placementRecorder) Revoke() {}

func (placementRecorder) RecordPlacement(session.Placement) error { return nil }
