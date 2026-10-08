//go:build !linux && !darwin

package peercred

import "net"

// unixConnPeerPID fails loudly on a platform with no kernel peer-pid read, so
// a caller is never handed a fabricated zero that reads as "no requester".
func unixConnPeerPID(_ *net.UnixConn) (int, error) {
	return 0, ErrUnsupportedPlatform
}
