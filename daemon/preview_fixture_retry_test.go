package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

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

// retryPreviewFixtureBind is initially the fixture's historical one-shot bind.
// The tests above pin the bounded retry behavior before the fixture adopts it.
func retryPreviewFixtureBind(_ int, bind func() error) error {
	return bind()
}
