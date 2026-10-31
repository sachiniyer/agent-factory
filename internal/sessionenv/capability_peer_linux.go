//go:build linux

package sessionenv

import (
	"bytes"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func peerPID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil || cred == nil {
		return 0, fmt.Errorf("SO_PEERCRED: %v", serr)
	}
	return int(cred.Pid), nil
}

// platformExeIdentity reads /proc/<pid>/exe. The " (deleted)" suffix is trusted
// only when the file really has no links: a live file literally named
// "af (deleted)" reads identically, and nlink==0 is what tells them apart.
func platformExeIdentity(pid int) (exeID, error) {
	link := fmt.Sprintf("/proc/%d/exe", pid)
	target, err := os.Readlink(link)
	if err != nil {
		return exeID{}, err
	}
	var st unix.Stat_t
	if err := unix.Stat(link, &st); err != nil {
		return exeID{}, err
	}
	return normalizeExeLink(target, uint64(st.Nlink), st.Ino), nil
}

// perMessageAttestation: the kernel attaches the SENDER's pid, uid and gid to
// every message on a socket whose receiver set SO_PASSCRED, and an unprivileged
// sender cannot name any pid but its own (a forked child cannot borrow its
// parent's). That is what makes the check per message rather than per connection.
const perMessageAttestation = true

func enableAttestation(conn net.Conn) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PASSCRED, 1)
	}); err != nil {
		return err
	}
	return serr
}

// readAttestedLine reads one newline-terminated message and returns the pid the
// kernel attested for it. Every read must carry credentials and they must all
// name the same sender, so a message cannot be stitched from two writers.
func readAttestedLine(conn net.Conn) ([]byte, int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, 0, fmt.Errorf("not a unix connection")
	}
	var line []byte
	sender := 0
	buf := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(unix.SizeofUcred)*4)
	for len(line) < claimMaxMessage {
		n, oobn, flags, _, err := uc.ReadMsgUnix(buf, oob)
		// ReadMsgUnix installs any SCM_RIGHTS descriptors in THIS process. Close
		// every one before anything else: a peer must not be able to exhaust the
		// broker's descriptors by attaching them, and a message that carries any
		// is refused.
		rights := closeReceivedRights(oob[:oobn])
		if rights > 0 || flags&unix.MSG_CTRUNC != 0 {
			return nil, 0, fmt.Errorf("message carries file descriptors or truncated ancillary data; refused")
		}
		if n > 0 {
			msgs, perr := unix.ParseSocketControlMessage(oob[:oobn])
			if perr != nil {
				return nil, 0, perr
			}
			pid := 0
			for i := range msgs {
				if cred, cerr := unix.ParseUnixCredentials(&msgs[i]); cerr == nil {
					pid = int(cred.Pid)
				}
			}
			if pid <= 0 || (sender != 0 && pid != sender) {
				return nil, 0, fmt.Errorf("message has no consistent kernel-attested sender")
			}
			sender = pid
			line = append(line, buf[:n]...)
			if i := bytes.IndexByte(line, '\n'); i >= 0 {
				return line[:i], sender, nil
			}
		}
		if err != nil {
			return nil, 0, err
		}
	}
	return nil, 0, fmt.Errorf("message too long")
}

// closeReceivedRights closes every descriptor delivered in oob and reports how
// many there were.
func closeReceivedRights(oob []byte) int {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return 0
	}
	closed := 0
	for i := range msgs {
		if msgs[i].Header.Level != unix.SOL_SOCKET || msgs[i].Header.Type != unix.SCM_RIGHTS {
			continue
		}
		fds, perr := unix.ParseUnixRights(&msgs[i])
		if perr != nil {
			continue
		}
		for _, fd := range fds {
			_ = unix.Close(fd)
			closed++
		}
	}
	return closed
}
