//go:build darwin

package sessionenv

import (
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// A live client's claim is never refused because the kernel could not answer
// LOCAL_PEERPID (#6455). Every claim ends with the server writing its reply and
// closing; a client that reads the peer's identity after sending the request can
// land after that close and get ENOTCONN. Many concurrent claims make that
// window common, so this is the closest a black-box test gets to the failure;
// TestDarwin_PeerIdentityIsReadAtConnect pins the mechanism deterministically.
func TestDarwin_ManyConcurrentClaimsAreNeverRefusedForTheirPeerIdentity(t *testing.T) {
	const workers, perWorker = 16, 40
	b, sock := startBroker(t, paneOf("s", os.Getpid()))
	t.Setenv("TMUX_PANE", "%1")
	tokens := make(chan string, workers*perWorker)
	for i := 0; i < workers*perWorker; i++ {
		token, err := b.Mint(testCap())
		require.NoError(t, err)
		tokens <- token
	}
	close(tokens)
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for token := range tokens {
				if _, err := ClaimCapability(sock, token); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}
