package proctree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"syscall"
)

// This file encodes and decodes the Linux NETLINK_SOCK_DIAG exchange that reads
// ONE unix socket by its inode (#7536): an exact lookup, not a dump. Like
// vnodepathinfo.go it carries no build tag, so the byte layout is tested on
// every platform; only unixsocket_linux.go sends it.
//
// The layouts, from include/uapi/linux/netlink.h, sock_diag.h and unix_diag.h:
//
//	struct nlmsghdr      { __u32 nlmsg_len; __u16 nlmsg_type; __u16 nlmsg_flags; __u32 nlmsg_seq; __u32 nlmsg_pid; };   // 16
//	struct unix_diag_req { __u8 sdiag_family; __u8 sdiag_protocol; __u16 pad; __u32 udiag_states;
//	                       __u32 udiag_ino; __u32 udiag_show; __u32 udiag_cookie[2]; };                            // 24
//	struct unix_diag_msg { __u8 udiag_family; __u8 udiag_type; __u8 udiag_state; __u8 pad;
//	                       __u32 udiag_ino; __u32 udiag_cookie[2]; };                                              // 16
//	struct rtattr        { unsigned short rta_len; unsigned short rta_type; };                                     // 4, payload 4-aligned
//
// With udiag_ino set and no NLM_F_DUMP, unix_diag_get_exact looks the socket
// up by inode in the requester's network namespace and answers with one
// unix_diag_msg, or with NLMSG_ERROR: -ENOENT when it has no such socket.
// udiag_cookie of all ones is INET_DIAG_NOCOOKIE, which skips the cookie check.
// UDIAG_SHOW_NAME asks for UNIX_DIAG_NAME, the bound address: a filesystem
// path, or for an abstract socket a NUL followed by the name.
//
// Measured on Linux 6.17 against a tmux 3.4 server's listener: state 10
// (TCP_LISTEN), name "./d.sock\x00", both before and after the socket file was
// removed; an inode with no socket answered ENOENT.
const (
	sockDiagByFamily  = 20 // SOCK_DIAG_BY_FAMILY
	netlinkSockDiag   = 4  // NETLINK_SOCK_DIAG
	nlmsgError        = 2  // NLMSG_ERROR
	nlmFRequest       = 1  // NLM_F_REQUEST
	nlmsgHdrLen       = 16
	unixDiagReqLen    = 24
	unixDiagMsgLen    = 16
	unixDiagShowName  = 0x1 // UDIAG_SHOW_NAME
	unixDiagAttrName  = 0   // UNIX_DIAG_NAME
	unixDiagListening = 10  // TCP_LISTEN, the state of a listening unix socket
	afUnixLinux       = 1   // AF_UNIX
)

// errUnixDiagNoSuchSocket: the kernel answered that it has no unix socket with
// that inode in this network namespace.
var errUnixDiagNoSuchSocket = errors.New("sock_diag: no unix socket with that inode")

// unixDiagRequest is the exact-lookup request for inode ino.
func unixDiagRequest(seq, ino uint32) []byte {
	req := make([]byte, nlmsgHdrLen+unixDiagReqLen)
	binary.LittleEndian.PutUint32(req[0:], uint32(len(req)))
	binary.LittleEndian.PutUint16(req[4:], sockDiagByFamily)
	binary.LittleEndian.PutUint16(req[6:], nlmFRequest)
	binary.LittleEndian.PutUint32(req[8:], seq)
	body := req[nlmsgHdrLen:]
	body[0] = afUnixLinux
	binary.LittleEndian.PutUint32(body[4:], 0xffffffff) // udiag_states: every state
	binary.LittleEndian.PutUint32(body[8:], ino)
	binary.LittleEndian.PutUint32(body[12:], unixDiagShowName)
	binary.LittleEndian.PutUint32(body[16:], 0xffffffff) // INET_DIAG_NOCOOKIE
	binary.LittleEndian.PutUint32(body[20:], 0xffffffff)
	return req
}

// unixDiagAnswer is one decoded exact-lookup reply.
type unixDiagAnswer struct {
	listening bool
	path      string // "" for an unbound socket
	abstract  bool
}

// parseUnixDiagReply decodes the reply to the request with seq for inode ino,
// sent to this socket's netlink port id portid. Anything that is not exactly
// that answer is an error, so a confused exchange reads as "could not tell",
// never as a socket: a reply to another request, another port, inode or
// family, or a malformed message. That includes one whose length is not
// 4-aligned at the end of the buffer (#7543 review R1). The step to the next
// message is clamped to the buffer, so no input can slice past it.
//
// The kernel addresses every reply to the requester's port: measured on Linux
// 6.17, a sock_diag reply's nlmsg_pid is the requesting socket's own port id,
// for an answer and for an error alike (#7543 review R4).
func parseUnixDiagReply(data []byte, seq, ino, portid uint32) (unixDiagAnswer, error) {
	for len(data) >= nlmsgHdrLen {
		msgLen := int(binary.LittleEndian.Uint32(data[0:]))
		if msgLen < nlmsgHdrLen || msgLen > len(data) {
			return unixDiagAnswer{}, fmt.Errorf("sock_diag: malformed message length %d", msgLen)
		}
		typ := binary.LittleEndian.Uint16(data[4:])
		gotSeq := binary.LittleEndian.Uint32(data[8:])
		gotPort := binary.LittleEndian.Uint32(data[12:])
		payload := data[nlmsgHdrLen:msgLen]
		data = data[min((msgLen+3)&^3, len(data)):]
		if gotSeq != seq {
			return unixDiagAnswer{}, fmt.Errorf("sock_diag: reply to request %d, want %d", gotSeq, seq)
		}
		if gotPort != portid {
			return unixDiagAnswer{}, fmt.Errorf("sock_diag: reply addressed to port %d, want %d", gotPort, portid)
		}
		switch typ {
		case nlmsgError:
			if len(payload) < 4 {
				return unixDiagAnswer{}, errors.New("sock_diag: short error message")
			}
			errno := -int32(binary.LittleEndian.Uint32(payload))
			if errno == int32(syscall.ENOENT) {
				return unixDiagAnswer{}, errUnixDiagNoSuchSocket
			}
			return unixDiagAnswer{}, fmt.Errorf("sock_diag: %w", syscall.Errno(errno))
		case sockDiagByFamily:
			return decodeUnixDiagMsg(payload, ino)
		default:
			return unixDiagAnswer{}, fmt.Errorf("sock_diag: unexpected message type %d", typ)
		}
	}
	return unixDiagAnswer{}, errors.New("sock_diag: no answer in the reply")
}

func decodeUnixDiagMsg(payload []byte, ino uint32) (unixDiagAnswer, error) {
	if len(payload) < unixDiagMsgLen {
		return unixDiagAnswer{}, errors.New("sock_diag: short unix_diag_msg")
	}
	if payload[0] != afUnixLinux {
		return unixDiagAnswer{}, fmt.Errorf("sock_diag: answer for family %d", payload[0])
	}
	if got := binary.LittleEndian.Uint32(payload[4:]); got != ino {
		return unixDiagAnswer{}, fmt.Errorf("sock_diag: answer for inode %d, want %d", got, ino)
	}
	a := unixDiagAnswer{listening: payload[2] == unixDiagListening}
	for attrs := payload[unixDiagMsgLen:]; len(attrs) >= 4; {
		l := int(binary.LittleEndian.Uint16(attrs[0:]))
		typ := binary.LittleEndian.Uint16(attrs[2:])
		if l < 4 || l > len(attrs) {
			return unixDiagAnswer{}, fmt.Errorf("sock_diag: malformed attribute length %d", l)
		}
		if typ == unixDiagAttrName {
			a.path, a.abstract = unixDiagName(attrs[4:l])
		}
		next := (l + 3) &^ 3
		if next >= len(attrs) {
			break
		}
		attrs = attrs[next:]
	}
	return a, nil
}

// unixDiagName renders UNIX_DIAG_NAME the way /proc/net/unix prints it: an
// abstract name (a leading NUL) as "@name" with any other NUL as '@', and a
// filesystem path up to its terminating NUL.
func unixDiagName(raw []byte) (path string, abstract bool) {
	if len(raw) == 0 {
		return "", false
	}
	if raw[0] == 0 {
		return "@" + strings.ReplaceAll(string(raw[1:]), "\x00", "@"), true
	}
	if i := strings.IndexByte(string(raw), 0); i >= 0 {
		raw = raw[:i]
	}
	return string(raw), false
}
