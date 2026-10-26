//go:build !linux

package session

import (
	"fmt"
	"net"
)

// socketPeerPid is the test seam over the platform's "who listens on this
// unix socket" answer. Where SO_PEERCRED does not exist (macOS, Windows)
// there is no portable way to name the answering process, so the capacity
// namespace proof stays unproven — which is the correct posture anyway:
// those engines never share af's mount namespace (#5382 review).
var socketPeerPid = platformSocketPeerPid

func platformSocketPeerPid(net.Conn) (int, error) {
	return 0, fmt.Errorf("peer credentials are unavailable on this platform")
}
