package daemon

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

const previewFixtureBindAttempts = 4

var errPreviewFixtureListenerUnbound = errors.New("preview listener did not bind")

func TestRetryPreviewFixtureBindRecoversFromTransientFailures(t *testing.T) {
	transient := errors.New("address already in use")
	attempts := 0

	err := retryPreviewFixtureBind(4, func() error {
		attempts++
		if attempts <= 2 {
			return transient
		}
		return nil
	})

	require.NoError(t, err)
	require.Equal(t, 3, attempts, "the successful bind must stop the retry loop")
}

func TestRetryPreviewFixtureBindStopsAtBound(t *testing.T) {
	transient := errors.New("address already in use")
	attempts := 0

	err := retryPreviewFixtureBind(3, func() error {
		attempts++
		return transient
	})

	require.ErrorIs(t, err, transient)
	require.ErrorContains(t, err, "after 3 attempts")
	require.Equal(t, 3, attempts, "a persistent bind failure must not loop forever")
}

// retryPreviewFixtureBind retries the complete reserve-and-bind attempt. Retrying
// only the initial :0 reservation would leave the release/rebind race unchanged.
func retryPreviewFixtureBind(maxAttempts int, bind func() error) error {
	if maxAttempts < 1 {
		return fmt.Errorf("preview listener bind has invalid attempt bound %d", maxAttempts)
	}
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err = bind(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("preview listener did not bind after %d attempts: %w", maxAttempts, err)
}
