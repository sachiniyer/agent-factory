//go:build !linux

package session

import (
	"fmt"
	"net"
)

// socketPeerCred is the test seam over the platform's "who listens on this
// unix socket" answer. Where SO_PEERCRED does not exist (macOS, Windows)
// there is no portable way to name the answering process, so the data-root
// identity proof stays unproven — which is the correct posture anyway:
// those engines never share af's view of the data root (#5382 review).
var socketPeerCred = platformSocketPeerCred

func platformSocketPeerCred(net.Conn) (pid, uid int, err error) {
	return 0, 0, fmt.Errorf("peer credentials are unavailable on this platform")
}
