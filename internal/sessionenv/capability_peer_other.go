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

// perMessageAttestation is false: macOS offers LOCAL_PEERPID (per connection)
// and no per-message sender attestation, so the answering process cannot be
// told apart from the process that created the socket. The check below is
// therefore only as strong as per-connection identity: a process that binds the
// socket, forks a child that serves it, and execs the real af in the creator
// defeats it. Documented residual (#4731); nothing here pretends otherwise.
const perMessageAttestation = false

type peerState struct{}

func enableAttestation(net.Conn) (*peerState, error) { return &peerState{}, nil }

func readAttestedLine(conn net.Conn, _ *peerState) ([]byte, int, error) {
	pid, err := peerPID(conn)
	if err != nil {
		return nil, 0, err
	}
	var line []byte
	one := make([]byte, 1)
	for len(line) < claimMaxMessage {
		if _, err := conn.Read(one); err != nil {
			return nil, 0, err
		}
		if one[0] == '\n' {
			return line, pid, nil
		}
		line = append(line, one[0])
	}
	return nil, 0, fmt.Errorf("message too long")
}
