package proctree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// UnixListenerPaths returns the filesystem path each LISTENING unix-domain
// socket that pid holds was bound to (#7533). The kernel records that address
// at bind time and keeps it for the socket's life, so it still names the path
// after the socket file is unlinked. That is what lets the socket-absent probe
// ask which tmux server is bound to a missing socket, rather than signalling
// every server. A path bound relative to the process's working directory comes
// back resolved against its current one, which is where the process itself
// would rebind it.
//
// Only listening sockets count: a tmux server's accepted connections carry its
// path too, but the listener is the socket it recreates. Abstract-namespace
// sockets (Linux) and unbound sockets have no filesystem path and are left out.
//
// An error means the descriptor table, the socket table, or a needed working
// directory could not be read completely. It is never "holds none". A
// descriptor that closes between the listing and its read is no longer held and
// is skipped, the same as OpenFilePaths. An empty result is a complete read
// that found no listener, which a caller must not read as "bound elsewhere"
// either: it names nothing to compare.
//
// Linux reads /proc/<pid>/fd and /proc/<pid>/net/unix; darwin reads
// proc_pidfdinfo(PROC_PIDFDSOCKETINFO). Every other platform answers
// ErrUnsupportedPlatform.
func UnixListenerPaths(pid int) ([]string, error) {
	paths, err := unixListenerPaths(pid)
	if err != nil {
		return nil, err
	}
	return resolveListenerPaths(paths, func() (string, bool) { return WorkingDir(pid) })
}

// errNoWorkingDir: a relative bound path, and the process's working directory
// could not be read to resolve it.
var errNoWorkingDir = errors.New("a socket is bound to a relative path and the process's working directory cannot be read")

// resolveListenerPaths makes every relative path absolute against the working
// directory cwd reports, read at most once and only when needed.
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
			p = filepath.Join(dir, p)
		}
		out = append(out, filepath.Clean(p))
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
//	offsetof(psi.soi_stat.vst_mode)         = 28
//	offsetof(psi.soi_type)                  = 176
//	offsetof(psi.soi_family)                = 184
//	offsetof(psi.soi_options)               = 188
//	offsetof(psi.soi_kind)                  = 256
//	offsetof(psi.soi_proto.pri_un.unsi_addr.ua_sun.sun_len)    = 280
//	offsetof(psi.soi_proto.pri_un.unsi_addr.ua_sun.sun_family) = 281
//	offsetof(psi.soi_proto.pri_un.unsi_addr.ua_sun.sun_path)   = 282
const (
	socketFdInfoSize         = 792
	socketFdInfoModeOffset   = 28
	socketFdInfoTypeOffset   = 176
	socketFdInfoFamilyOffset = 184
	socketFdInfoOptsOffset   = 188
	socketFdInfoKindOffset   = 256
	socketFdInfoSunLenOffset = 280
	socketFdInfoSunFamOffset = 281
	socketFdInfoSunPathOff   = 282
	darwinSunPathLen         = 104

	darwinAFUnix        = 1      // AF_UNIX
	darwinSockinfoUn    = 3      // SOCKINFO_UN
	darwinSoAcceptConn  = 0x0002 // SO_ACCEPTCONN
	darwinSIFMT         = 0o170000
	darwinSIFSOCK       = 0o140000
	darwinSockStream    = 1 // SOCK_STREAM
	darwinSockDgram     = 2 // SOCK_DGRAM
	darwinSockSeqpacket = 5 // SOCK_SEQPACKET
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
//   - soi_stat.vst_mode says S_IFSOCK;
//   - an AF_UNIX socket's soi_kind is SOCKINFO_UN, and its soi_type is a unix
//     socket type;
//   - a bound address has sun_family AF_UNIX, a length that fits sockaddr_un,
//     and a path that is NUL-terminated within sun_path and free of control
//     bytes.
//
// A refusal is the caller's "could not read", which leaves the process
// unresolved.
func unixSocketFromSocketFdInfo(buf []byte) (darwinUnixSocket, error) {
	if len(buf) != socketFdInfoSize {
		return darwinUnixSocket{}, fmt.Errorf("socket_fdinfo is %d bytes, want %d", len(buf), socketFdInfoSize)
	}
	if mode := binary.LittleEndian.Uint16(buf[socketFdInfoModeOffset:]); uint32(mode)&darwinSIFMT != darwinSIFSOCK {
		return darwinUnixSocket{}, fmt.Errorf("socket_fdinfo: vst_mode %#o is not a socket", mode)
	}
	family := int32(binary.LittleEndian.Uint32(buf[socketFdInfoFamilyOffset:]))
	kind := int32(binary.LittleEndian.Uint32(buf[socketFdInfoKindOffset:]))
	if family != darwinAFUnix {
		if kind == darwinSockinfoUn {
			return darwinUnixSocket{}, fmt.Errorf("socket_fdinfo: soi_kind SOCKINFO_UN with soi_family %d", family)
		}
		return darwinUnixSocket{}, nil
	}
	if kind != darwinSockinfoUn {
		return darwinUnixSocket{}, fmt.Errorf("socket_fdinfo: AF_UNIX with soi_kind %d", kind)
	}
	switch typ := int32(binary.LittleEndian.Uint32(buf[socketFdInfoTypeOffset:])); typ {
	case darwinSockStream, darwinSockDgram, darwinSockSeqpacket:
	default:
		return darwinUnixSocket{}, fmt.Errorf("socket_fdinfo: AF_UNIX with soi_type %d", typ)
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
	if sunLen > 2+darwinSunPathLen {
		return darwinUnixSocket{}, fmt.Errorf("socket_fdinfo: sun_len %d exceeds sockaddr_un", sunLen)
	}
	raw := buf[socketFdInfoSunPathOff : socketFdInfoSunPathOff+darwinSunPathLen]
	end := bytes.IndexByte(raw, 0)
	if end < 0 {
		return darwinUnixSocket{}, errors.New("socket_fdinfo: sun_path is not NUL-terminated")
	}
	for _, c := range raw[:end] {
		if c < 0x20 || c == 0x7f {
			return darwinUnixSocket{}, errors.New("socket_fdinfo: sun_path holds a control byte")
		}
	}
	s.path = string(raw[:end])
	return s, nil
}
