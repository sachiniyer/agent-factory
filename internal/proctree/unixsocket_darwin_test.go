//go:build darwin

package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The PROC_PIDFDSOCKETINFO layout unixsocket.go transcribes, checked against
// the macOS SDK's own <sys/proc_info.h> on the runner rather than against the
// transcription (#7533). A program compiled from the real header prints every
// size and offset the decoder reads, and each must equal its constant. Where no
// C compiler is available the test cannot confirm the layout; on CI that is a
// failure, not a skip, because the decoder must not run on an unconfirmed
// layout there.
func TestSocketFdInfoLayoutMatchesTheSDKHeader(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("no C compiler on the runner, so the socket_fdinfo layout cannot be confirmed: %v", err)
		}
		t.Skipf("no C compiler: %v", err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "layout.c")
	require.NoError(t, os.WriteFile(src, []byte(`#include <stddef.h>
#include <stdio.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/stat.h>
#include <sys/proc_info.h>
#define P(name, v) printf("%s %ld\n", name, (long)(v))
int main(void) {
	P("size", sizeof(struct socket_fdinfo));
	P("type", offsetof(struct socket_fdinfo, psi.soi_type));
	P("family", offsetof(struct socket_fdinfo, psi.soi_family));
	P("options", offsetof(struct socket_fdinfo, psi.soi_options));
	P("kind", offsetof(struct socket_fdinfo, psi.soi_kind));
	P("sun_len", offsetof(struct socket_fdinfo, psi.soi_proto.pri_un.unsi_addr.ua_sun.sun_len));
	P("sun_family", offsetof(struct socket_fdinfo, psi.soi_proto.pri_un.unsi_addr.ua_sun.sun_family));
	P("sun_path", offsetof(struct socket_fdinfo, psi.soi_proto.pri_un.unsi_addr.ua_sun.sun_path));
	P("sun_path_len", sizeof(((struct sockaddr_un *)0)->sun_path));
	P("PROC_PIDFDSOCKETINFO", PROC_PIDFDSOCKETINFO);
	P("PROX_FDTYPE_SOCKET", PROX_FDTYPE_SOCKET);
	P("SOCKINFO_GENERIC", SOCKINFO_GENERIC);
	P("SOCKINFO_UN", SOCKINFO_UN);
	P("AF_UNIX", AF_UNIX);
	P("SO_ACCEPTCONN", SO_ACCEPTCONN);
	P("SOCK_STREAM", SOCK_STREAM);
	P("SOCK_DGRAM", SOCK_DGRAM);
	P("SOCK_SEQPACKET", SOCK_SEQPACKET);
	return 0;
}
`), 0o644))
	bin := filepath.Join(dir, "layout")
	out, err := exec.Command(cc, "-o", bin, src).CombinedOutput()
	require.NoError(t, err, "compile against <sys/proc_info.h>: %s", out)
	out, err = exec.Command(bin).Output()
	require.NoError(t, err)

	sdk := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, value, ok := strings.Cut(line, " ")
		require.True(t, ok, "line %q", line)
		n, err := strconv.Atoi(value)
		require.NoError(t, err, "line %q", line)
		sdk[name] = n
	}
	t.Logf("socket_fdinfo per the SDK header: %v", sdk)
	for name, want := range map[string]int{
		"size":                 socketFdInfoSize,
		"type":                 socketFdInfoTypeOffset,
		"family":               socketFdInfoFamilyOffset,
		"options":              socketFdInfoOptsOffset,
		"kind":                 socketFdInfoKindOffset,
		"sun_len":              socketFdInfoSunLenOffset,
		"sun_family":           socketFdInfoSunFamOffset,
		"sun_path":             socketFdInfoSunPathOff,
		"sun_path_len":         darwinSunPathLen,
		"PROC_PIDFDSOCKETINFO": procPIDFdSocketInfo,
		"PROX_FDTYPE_SOCKET":   procFdTypeSocket,
		"SOCKINFO_GENERIC":     darwinSockinfoGeneric,
		"SOCKINFO_UN":          darwinSockinfoUn,
		"AF_UNIX":              darwinAFUnix,
		"SO_ACCEPTCONN":        darwinSoAcceptConn,
		"SOCK_STREAM":          darwinSockStream,
		"SOCK_DGRAM":           darwinSockDgram,
		"SOCK_SEQPACKET":       darwinSockSeqpacket,
	} {
		got, ok := sdk[name]
		require.True(t, ok, "the SDK program did not print %s", name)
		require.Equal(t, want, got, "%s: the decoder's constant differs from <sys/proc_info.h>", name)
	}
}
