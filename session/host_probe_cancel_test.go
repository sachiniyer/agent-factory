package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cancelling an advisory probe's context kills its ssh process group promptly
// rather than waiting out the 30s bound (#5839).
func TestProvisionerRunKillsTheChildWhenItsContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// `sh -c 'sleep 30' x <script>` stands in for a hung ssh: it ignores the script.
	p := &sandboxProvisioner{sshCmd: `sh -c 'sleep 30' x`, runCtx: ctx}
	done := make(chan error, 1)
	go func() {
		_, err := p.runCommand(30*time.Second, "true", nil, false)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the probe context did not kill its ssh child")
	}
}
