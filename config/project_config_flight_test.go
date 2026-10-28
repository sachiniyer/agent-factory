package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompletedCheckoutMarkerFlightIsEvictedBeforeWaitersWake(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	markerName, err := checkoutMarkerName()
	require.NoError(t, err)
	marker := filepath.Join(repoRoot, ".git", checkoutMarkerDirName, markerName)

	evictedBeforeWake := make(chan bool, 1)
	var flight *checkoutMarkerReadFlight
	flight = &checkoutMarkerReadFlight{
		done: make(chan struct{}),
		beforeWakeForTest: func() {
			cached, loaded := checkoutMarkerReadFlights.Load(marker)
			evictedBeforeWake <- !loaded || cached != flight
		},
	}
	checkoutMarkerReadFlights.Store(marker, flight)
	t.Cleanup(func() { checkoutMarkerReadFlights.CompareAndDelete(marker, flight) })

	go completeCheckoutMarkerRead(marker, flight)
	require.True(t, <-evictedBeforeWake,
		"a completed marker read must leave the flight table before its waiters can revalidate the checkout")
	<-flight.done
	require.NoError(t, flight.result.err)
	require.True(t, flight.result.exists)
	require.Equal(t, project.CheckoutID, flight.result.id)
}
