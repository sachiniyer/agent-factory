//go:build !windows && !linux && !darwin

package sessionenv

import (
	"fmt"
	"net"
)

func peerPID(net.Conn) (int, error) {
	return 0, fmt.Errorf("peer credentials are unavailable on this platform")
}

func platformExeIdentity(int) (exeID, error) {
	return exeID{}, fmt.Errorf("process executable identity is unavailable on this platform")
}
