package proctree

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The /proc/net/unix parse, on rows no live process on the runner would
// produce: a listener whose path holds spaces, the accepted connection beside
// it, an abstract listener, an unbound one, and sockets this process does not
// hold.
func TestParseNetUnixListeners(t *testing.T) {
	const header = "Num       RefCount Protocol Flags    Type St Inode Path\n"
	table := header +
		"0000000000000000: 00000002 00000000 00010000 0001 01 1001 /tmp/tmux-1000/my server\n" +
		"0000000000000000: 00000003 00000000 00000000 0001 03 1002 /tmp/tmux-1000/my server\n" +
		"0000000000000000: 00000002 00000000 00010000 0001 01 1003 @/tmp/.X11-unix/X0\n" +
		"0000000000000000: 00000002 00000000 00010000 0001 01 1004\n" +
		"0000000000000000: 00000002 00000000 00010000 0001 01 1005 ./rel.sock\n" +
		"0000000000000000: 00000002 00000000 00010000 0001 01 2001 /run/not-ours.sock\n" +
		"0000000000000000: 00000003 00000000 00000000 0001 03 1006 /run/bound-not-listening\n"
	held := map[string]bool{"1001": true, "1002": true, "1003": true, "1004": true, "1005": true, "1006": true}

	paths, err := parseNetUnixListeners([]byte(table), held)
	require.NoError(t, err)
	require.Equal(t, []string{"/tmp/tmux-1000/my server", "./rel.sock"}, paths,
		"only held listeners with a filesystem path, spaces kept")

	none, err := parseNetUnixListeners([]byte(header), held)
	require.NoError(t, err)
	require.Empty(t, none)

	for name, bad := range map[string]string{
		"no header": "0000000000000000: 00000002 00000000 00010000 0001 01 1001 /x\n",
		"short row": header + "0000000000000000: 00000002 00000000 00010000\n",
		"bad flags": header + "0000000000000000: 00000002 00000000 zz010000 0001 01 1001 /x\n",
	} {
		_, err := parseNetUnixListeners([]byte(bad), held)
		require.Error(t, err, name)
	}
}

// A relative bound path resolves against the working directory, which is read
// once and only when needed; an unreadable one makes the whole read fail.
func TestResolveListenerPaths(t *testing.T) {
	reads := 0
	cwd := func() (string, bool) { reads++; return "/home/u/proj", true }
	got, err := resolveListenerPaths([]string{"/tmp/a/../s", "./r.sock", "x/y"}, cwd)
	require.NoError(t, err)
	require.Equal(t, []string{"/tmp/s", "/home/u/proj/r.sock", "/home/u/proj/x/y"}, got)
	require.Equal(t, 1, reads)

	reads = 0
	_, err = resolveListenerPaths([]string{"/tmp/s"}, cwd)
	require.NoError(t, err)
	require.Zero(t, reads, "an absolute path needs no working directory")

	_, err = resolveListenerPaths([]string{"./r.sock"}, func() (string, bool) { return "", false })
	require.ErrorIs(t, err, errNoWorkingDir)
	_, err = resolveListenerPaths([]string{"./r.sock"}, func() (string, bool) { return "proj", true })
	require.ErrorIs(t, err, errNoWorkingDir, "a relative working directory resolves nothing")
}

// socketFdInfo builds a PROC_PIDFDSOCKETINFO buffer from its own table of
// offsets, written out independently of the constants the decoder uses, so a
// typo in either fails here instead of decoding a different path.
func socketFdInfo(typ, family, kind int32, options uint16, sunLen, sunFamily byte, path string) []byte {
	buf := make([]byte, 792)
	binary.LittleEndian.PutUint32(buf[176:], uint32(typ))
	binary.LittleEndian.PutUint32(buf[184:], uint32(family))
	binary.LittleEndian.PutUint16(buf[188:], options)
	binary.LittleEndian.PutUint32(buf[256:], uint32(kind))
	buf[280] = sunLen
	buf[281] = sunFamily
	copy(buf[282:280+255], path) // the address union is SOCK_MAXADDRLEN bytes
	return buf
}

func TestUnixSocketFromSocketFdInfo(t *testing.T) {
	const unix, un, stream, accept = 1, 3, 1, 0x0002
	path := "/private/tmp/tmux-501/default"

	got, err := unixSocketFromSocketFdInfo(socketFdInfo(stream, unix, un, accept, 106, unix, path))
	require.NoError(t, err)
	require.Equal(t, darwinUnixSocket{unix: true, listening: true, path: path}, got)

	got, err = unixSocketFromSocketFdInfo(socketFdInfo(stream, unix, un, 0, 0, 0, ""))
	require.NoError(t, err)
	require.Equal(t, darwinUnixSocket{unix: true}, got, "an unbound socket has no path")

	got, err = unixSocketFromSocketFdInfo(socketFdInfo(stream, 2, 2, accept, 0, 0, ""))
	require.NoError(t, err)
	require.Equal(t, darwinUnixSocket{}, got, "an AF_INET socket is not a unix socket")

	got, err = unixSocketFromSocketFdInfo(socketFdInfo(stream, unix, 0, accept, 0, 0, ""))
	require.NoError(t, err)
	require.Equal(t, darwinUnixSocket{unix: true}, got, "an AF_UNIX socket with no protocol block (SOCKINFO_GENERIC) has nothing to report")

	long := "/" + strings.Repeat("a", 120)
	got, err = unixSocketFromSocketFdInfo(socketFdInfo(stream, unix, un, accept, byte(2+len(long)), unix, long))
	require.NoError(t, err)
	require.Equal(t, long, got.path, "a binder may pass a sun_len past sockaddr_un; the path is bounded by sun_len")

	got, err = unixSocketFromSocketFdInfo(socketFdInfo(stream, unix, un, accept, byte(2+len(path)), unix, path+"trailing"))
	require.NoError(t, err)
	require.Equal(t, path, got.path, "without a NUL, sun_len ends the path")

	for name, buf := range map[string][]byte{
		"short buffer":                    socketFdInfo(stream, unix, un, accept, 106, unix, path)[:791],
		"AF_UNIX with another kind":       socketFdInfo(stream, unix, 2, accept, 106, unix, path),
		"SOCKINFO_UN without AF_UNIX":     socketFdInfo(stream, 2, un, accept, 106, unix, path),
		"not a unix socket type":          socketFdInfo(3, unix, un, accept, 106, unix, path),
		"bound address of another kind":   socketFdInfo(stream, unix, un, accept, 106, 2, path),
		"sun_len shorter than its header": socketFdInfo(stream, unix, un, accept, 1, unix, path),
		"sun_path with a control byte":    socketFdInfo(stream, unix, un, accept, 106, unix, "/tmp/a\x01b"),
	} {
		_, err := unixSocketFromSocketFdInfo(buf)
		require.Error(t, err, name)
	}
}
