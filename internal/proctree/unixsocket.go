package proctree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// UnixSocket is one unix-domain socket a process holds (#7536).
type UnixSocket struct {
	// Path is the address the socket is bound to, as the kernel spelled it: a
	// filesystem path, with a relative one prefixed by the process's current
	// working directory and never lexically cleaned; "@name" for an abstract
	// socket, the way /proc/net/unix prints it; "" for an unbound one.
	Path string
	// Abstract marks an abstract-namespace address, which names no file.
	Abstract bool
	// Listening marks a listening socket.
	Listening bool
}

// UnixSockets returns every unix-domain socket pid holds, each one read from
// the kernel on its own (#7536). The kernel records a socket's address at bind
// time and keeps it for the socket's life, so a bound path survives an unlink
// of the socket file.
//
// Each socket is read on its own because a verdict that RULES something OUT, or
// PROVES something, must rest on the socket itself. A walk of a whole socket
// table is not atomic and can miss a row, and a missed row is a socket the
// verdict never saw. The socket-absent probe rules a tmux server out with this
// ("bound elsewhere"), and the daemon's throwaway proof requires every path a
// server holds to lie in a private directory.
//
//   - Linux: for each fd in /proc/<pid>/fd that is a socket, the socket's
//     protocol name (the sockfs xattr system.sockprotoname: "UNIX", or
//     "UNIX-STREAM" since Linux 5.15) says whether it is a unix socket, and an
//     exact NETLINK_SOCK_DIAG lookup by its inode reads its state and address
//     (unixdiag.go). The lookup runs in this process's network namespace.
//   - darwin: proc_pidfdinfo(PROC_PIDFDSOCKETINFO) for each socket fd.
//   - elsewhere: ErrUnsupportedPlatform.
//
// An error means some socket pid holds could not be read, never "holds none".
// That includes a lookup the kernel cannot answer while the fd still holds the
// socket: unix_diag missing, refused, or a socket in another network
// namespace. A descriptor that closes, or turns into something else, between
// the listing and its read is no longer held and is skipped.
//
// A read is complete only if the process it began on is still the one
// running when it ends: the same pid with the same start stamp. A process
// that exits part-way closes its descriptors, and the reads after that find
// them gone and skip them as closed, which would pass a partial list off as a
// complete one (#7543 review R3). That answers ErrProcessExited instead.
func UnixSockets(pid int) ([]UnixSocket, error) {
	var socks []UnixSocket
	err := sameInstanceThroughout(pid, func() (err error) {
		socks, err = unixSockets(pid)
		return err
	})
	if err != nil {
		return nil, err
	}
	return resolveSocketPaths(socks, func() (string, bool) { return WorkingDir(pid) })
}

// instanceLookup is the process lookup sameInstanceThroughout brackets a read
// with. A package var so a test can make the process exit, or be replaced,
// mid-read; nothing else sets it.
var instanceLookup = Lookup

// sameInstanceThroughout runs read, and returns its error, only if pid names
// the same process instance (pid and start stamp) before and after it.
// Otherwise it answers ErrProcessExited: the process read is gone, so the read
// cannot be complete. A lookup that fails for any other reason is returned as
// it is, never as an exit.
func sameInstanceThroughout(pid int, read func() error) error {
	before, err := instanceLookup(pid)
	if err != nil {
		if processGone(err) {
			return ErrProcessExited
		}
		return err
	}
	readErr := read()
	after, err := instanceLookup(pid)
	switch {
	case err != nil && processGone(err):
		return ErrProcessExited
	case err != nil:
		return err
	case after.StartID != before.StartID:
		return ErrProcessExited
	}
	return readErr
}

// processGone reports whether a lookup error says the pid names no live
// process: ErrProcessExited (a zombie, or darwin's no-such-pid), the Linux
// stat file being gone, or the kernel's ESRCH whichever /proc read met it
// (#7695 R3 — a stat read interrupted by the exit can answer ESRCH, not
// ENOENT, and a pid the kernel answers ESRCH for is gone).
func processGone(err error) bool {
	return errors.Is(err, ErrProcessExited) || errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ESRCH)
}

// resolveSocketPaths prefixes each relative filesystem path with the working
// directory cwd reports, read at most once and only when needed. An unbound
// socket and an abstract one name no file and are left as they are.
func resolveSocketPaths(socks []UnixSocket, cwd func() (string, bool)) ([]UnixSocket, error) {
	var relative []string
	for _, s := range socks {
		if s.Path != "" && !s.Abstract && !filepath.IsAbs(s.Path) {
			relative = append(relative, s.Path)
		}
	}
	if len(relative) == 0 {
		return socks, nil
	}
	resolved, err := resolveListenerPaths(relative, cwd)
	if err != nil {
		return nil, err
	}
	for i := range socks {
		if s := socks[i]; s.Path != "" && !s.Abstract && !filepath.IsAbs(s.Path) {
			socks[i].Path, resolved = resolved[0], resolved[1:]
		}
	}
	return socks, nil
}

// UnixListenerPaths returns the filesystem path each LISTENING unix-domain
// socket that pid holds was bound to (#7533), from UnixSockets' per-socket
// read. That is what lets the socket-absent probe ask which tmux server is
// bound to a missing socket, rather than signalling every server.
//
// Only listening sockets count: a tmux server's accepted connections carry its
// path too, but the listener is the socket it recreates. Abstract-namespace
// and unbound sockets have no filesystem path and are left out.
//
// An error is UnixSockets' error, never "holds none". An empty result is a
// complete read that found no listener, which a caller must not read as
// "bound elsewhere" either: it names nothing to compare.
func UnixListenerPaths(pid int) ([]string, error) {
	socks, err := UnixSockets(pid)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, s := range socks {
		if s.Listening && s.Path != "" && !s.Abstract {
			paths = append(paths, s.Path)
		}
	}
	return paths, nil
}

// UnixListenerPathsByTable is UnixListenerPaths read from the socket TABLE,
// /proc/<pid>/net/unix, matched to the inodes in /proc/<pid>/fd: the reader
// #7533 shipped, kept for one use only (#7536). The walk is not atomic, so it
// can miss a row, and a missed row can only LOSE a listener, never invent one.
// So its answer is positive evidence and nothing more. It may find the server
// bound to a missing socket where UnixSockets cannot answer (no unix_diag,
// another network namespace), and it must never rule a server out or prove
// anything private. Linux only; elsewhere ErrUnsupportedPlatform.
func UnixListenerPathsByTable(pid int) ([]string, error) {
	var paths []string
	err := sameInstanceThroughout(pid, func() (err error) {
		paths, err = unixListenerPathsByTable(pid)
		return err
	})
	if err != nil {
		return nil, err
	}
	return resolveListenerPaths(paths, func() (string, bool) { return WorkingDir(pid) })
}

// errNoWorkingDir: a relative bound path, and the process's working directory
// could not be read to resolve it.
var errNoWorkingDir = errors.New("a socket is bound to a relative path and the process's working directory cannot be read")

// resolveListenerPaths makes every relative path absolute against the working
// directory cwd reports, read at most once and only when needed. It joins by
// concatenation and cleans nothing (see UnixSocket).
func resolveListenerPaths(paths []string, cwd func() (string, bool)) ([]string, error) {
	out := make([]string, 0, len(paths))
	dir, dirRead := "", false
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			if !dirRead {
				d, ok := cwd()
				if !ok || !filepath.IsAbs(d) {
					return nil, errNoWorkingDir
				}
				dir, dirRead = d, true
			}
			p = strings.TrimSuffix(dir, "/") + "/" + p
		}
		out = append(out, p)
	}
	return out, nil
}

// soAcceptConnLinux is __SO_ACCEPTCON, the flag /proc/net/unix prints in its
// Flags column for a listening socket (include/linux/net.h; printed by
// unix_seq_show in net/unix/af_unix.c). Measured on a tmux 3.4 server: the
// listener reads 00010000, an accepted connection to it 00000000, and both
// carry the server's path.
const soAcceptConnLinux = 0x10000

// parseNetUnixListeners returns the bound path of every listening socket in a
// /proc/<pid>/net/unix table whose inode is in inodes. Each row is
// `Num RefCount Protocol Flags Type St Inode [Path]`. The path is the rest of
// the row, spaces included, and an abstract one starts with '@'. Anything that
// is not that shape is an error, so a format change reads as "could not tell",
// never as "no listener".
func parseNetUnixListeners(data []byte, inodes map[string]bool) ([]string, error) {
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "Num") {
		return nil, errors.New("/proc/net/unix: unexpected header")
	}
	var paths []string
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields, rest := leadingFields(line, 7)
		if len(fields) < 7 {
			return nil, fmt.Errorf("/proc/net/unix: short row %q", line)
		}
		if !inodes[fields[6]] {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil {
			return nil, fmt.Errorf("/proc/net/unix: bad flags in row %q", line)
		}
		if flags&soAcceptConnLinux == 0 || rest == "" || strings.HasPrefix(rest, "@") {
			continue
		}
		paths = append(paths, rest)
	}
	return paths, nil
}

// leadingFields splits off the first n space-separated fields of line and
// returns them with the remainder after the separator that follows them,
// unmodified.
func leadingFields(line string, n int) (fields []string, rest string) {
	s := line
	for len(fields) < n {
		s = strings.TrimLeft(s, " ")
		if s == "" {
			return fields, ""
		}
		end := strings.IndexByte(s, ' ')
		if end < 0 {
			return append(fields, s), ""
		}
		fields = append(fields, s[:end])
		s = s[end:]
	}
	return fields, strings.TrimPrefix(s, " ")
}

// Layout of darwin's struct socket_fdinfo, the answer to
// proc_pidfdinfo(PROC_PIDFDSOCKETINFO), transcribed from xnu bsd/sys/proc_info.h:
//
//	struct socket_fdinfo { struct proc_fileinfo pfi; struct socket_info psi; };
//	struct proc_fileinfo { uint32_t fi_openflags, fi_status; off_t fi_offset;
//	                       int32_t fi_type; uint32_t fi_guardflags; };
//	struct socket_info {
//	        struct vinfo_stat soi_stat;          // see vnodepathinfo.go
//	        uint64_t soi_so, soi_pcb;
//	        int soi_type, soi_protocol, soi_family;
//	        short soi_options, soi_linger, soi_state, soi_qlen, soi_incqlen,
//	              soi_qlimit, soi_timeo;
//	        u_short soi_error; uint32_t soi_oobmark;
//	        struct sockbuf_info soi_rcv, soi_snd;  // 5 x uint32_t, 2 x short
//	        int soi_kind; uint32_t rfu_1;
//	        union { struct in_sockinfo pri_in; struct tcp_sockinfo pri_tcp;
//	                struct un_sockinfo pri_un; struct ndrv_info pri_ndrv;
//	                struct kern_event_info pri_kern_event;
//	                struct kern_ctl_info pri_kern_ctl;
//	                struct vsock_sockinfo pri_vsock; } soi_proto;
//	};
//	struct un_sockinfo { uint64_t unsi_conn_so, unsi_conn_pcb;
//	        union { struct sockaddr_un ua_sun; char ua_dummy[SOCK_MAXADDRLEN]; } unsi_addr;
//	        union { struct sockaddr_un ua_sun; char ua_dummy[SOCK_MAXADDRLEN]; } unsi_caddr; };
//	struct sockaddr_un { u_char sun_len; sa_family_t sun_family; char sun_path[104]; };
//
// unsi_addr is the socket's OWN bound address; unsi_caddr is its peer's, which a
// tmux client's socket carries, so only unsi_addr is read. Every member is a
// fixed-width scalar or a char array, so the layout is the same on arm64 and
// amd64. These were derived by compiling the transcribed declarations and
// printing sizeof/offsetof, and unixsocket_darwin_test.go compiles a program
// against the macOS SDK's own <sys/proc_info.h> on the runner and fails unless
// every number below matches it:
//
//	sizeof(struct socket_fdinfo)            = 792
//	offsetof(psi.soi_type)                  = 176
//	offsetof(psi.soi_family)                = 184
//	offsetof(psi.soi_options)               = 188
//	offsetof(psi.soi_kind)                  = 256
//	offsetof(psi.soi_proto.pri_un.unsi_addr.ua_sun.sun_len)    = 280
//	offsetof(psi.soi_proto.pri_un.unsi_addr.ua_sun.sun_family) = 281
//	offsetof(psi.soi_proto.pri_un.unsi_addr.ua_sun.sun_path)   = 282
const (
	socketFdInfoSize         = 792
	socketFdInfoTypeOffset   = 176
	socketFdInfoFamilyOffset = 184
	socketFdInfoOptsOffset   = 188
	socketFdInfoKindOffset   = 256
	socketFdInfoSunLenOffset = 280
	socketFdInfoSunFamOffset = 281
	socketFdInfoSunPathOff   = 282
	darwinSunPathLen         = 104

	darwinAFUnix          = 1      // AF_UNIX
	darwinSockinfoGeneric = 0      // SOCKINFO_GENERIC
	darwinSockinfoUn      = 3      // SOCKINFO_UN
	darwinSoAcceptConn    = 0x0002 // SO_ACCEPTCONN
	darwinSockStream      = 1      // SOCK_STREAM
	darwinSockDgram       = 2      // SOCK_DGRAM
	darwinSockSeqpacket   = 5      // SOCK_SEQPACKET
)

// darwinUnixSocket is one decoded socket_fdinfo.
type darwinUnixSocket struct {
	unix      bool   // an AF_UNIX socket
	listening bool   // SO_ACCEPTCONN is set
	path      string // its own bound path, "" when unbound
}

// unixSocketFromSocketFdInfo decodes a PROC_PIDFDSOCKETINFO buffer, reporting
// why it refuses rather than guessing. A mis-declared offset would not fail on
// its own; it would return a DIFFERENT PATH, and a wrong path can rule a tmux
// server out as the owner of a missing socket and license the destructive
// cleanup a missing socket otherwise refuses. So the decode checks facts that
// hold for every socket the kernel describes, and a layout that is not the one
// above breaks one of them:
//
//   - the buffer is exactly sizeof(struct socket_fdinfo);
//   - soi_kind SOCKINFO_UN only ever comes with soi_family AF_UNIX, and an
//     AF_UNIX socket's soi_type is a unix socket type;
//   - a bound address has sun_family AF_UNIX, a sun_len of at least its own
//     two header bytes, and a path within sun_len that is free of control
//     bytes.
//
// An AF_UNIX socket whose soi_kind is still SOCKINFO_GENERIC is one the kernel
// had no protocol block for (fill_socketinfo skips the per-protocol part then,
// as for a socket being torn down). It has no address to report and is skipped:
// a listener being read always has its block. sun_len is the length the binder
// passed, which may exceed sockaddr_un's 106 bytes up to SOCK_MAXADDRLEN, and
// the kernel copies that many bytes into the 255-byte union, so the path is
// bounded by sun_len.
//
// soi_stat is not one of them. The kernel leaves it zeroed for a socket:
// measured on the macOS 15.8 arm64 runner, every socket fd read vst_mode 0 at
// the offset the SDK header confirms (#7537).
//
// A refusal is the caller's "could not read", which leaves the process
// unresolved.
func unixSocketFromSocketFdInfo(buf []byte) (darwinUnixSocket, error) {
	if len(buf) != socketFdInfoSize {
		return darwinUnixSocket{}, fmt.Errorf("socket_fdinfo is %d bytes, want %d", len(buf), socketFdInfoSize)
	}
	family := int32(binary.LittleEndian.Uint32(buf[socketFdInfoFamilyOffset:]))
	kind := int32(binary.LittleEndian.Uint32(buf[socketFdInfoKindOffset:]))
	if family != darwinAFUnix {
		if kind == darwinSockinfoUn {
			return darwinUnixSocket{}, fmt.Errorf("socket_fdinfo: soi_kind SOCKINFO_UN with soi_family %d", family)
		}
		return darwinUnixSocket{}, nil
	}
	switch typ := int32(binary.LittleEndian.Uint32(buf[socketFdInfoTypeOffset:])); typ {
	case darwinSockStream, darwinSockDgram, darwinSockSeqpacket:
	default:
		return darwinUnixSocket{}, fmt.Errorf("socket_fdinfo: AF_UNIX with soi_type %d", typ)
	}
	switch kind {
	case darwinSockinfoUn:
	case darwinSockinfoGeneric:
		return darwinUnixSocket{unix: true}, nil
	default:
		return darwinUnixSocket{}, fmt.Errorf("socket_fdinfo: AF_UNIX with soi_kind %d", kind)
	}
	s := darwinUnixSocket{
		unix:      true,
		listening: binary.LittleEndian.Uint16(buf[socketFdInfoOptsOffset:])&darwinSoAcceptConn != 0,
	}
	sunLen := int(buf[socketFdInfoSunLenOffset])
	if sunLen == 0 {
		return s, nil
	}
	if fam := buf[socketFdInfoSunFamOffset]; fam != darwinAFUnix {
		return darwinUnixSocket{}, fmt.Errorf("socket_fdinfo: bound address has sun_family %d", fam)
	}
	if sunLen < 2 {
		return darwinUnixSocket{}, fmt.Errorf("socket_fdinfo: sun_len %d is shorter than its own header", sunLen)
	}
	raw := buf[socketFdInfoSunPathOff : socketFdInfoSunLenOffset+sunLen]
	end := bytes.IndexByte(raw, 0)
	if end < 0 {
		end = len(raw)
	}
	for _, c := range raw[:end] {
		if c < 0x20 || c == 0x7f {
			return darwinUnixSocket{}, errors.New("socket_fdinfo: sun_path holds a control byte")
		}
	}
	s.path = string(raw[:end])
	return s, nil
}
