package proctree

import (
	"encoding/binary"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// The exact-lookup request, byte for byte, from its own table of offsets.
func TestUnixDiagRequest(t *testing.T) {
	req := unixDiagRequest(7, 3051067119)
	require.Len(t, req, 40)
	require.EqualValues(t, 40, binary.LittleEndian.Uint32(req[0:]), "nlmsg_len")
	require.EqualValues(t, 20, binary.LittleEndian.Uint16(req[4:]), "SOCK_DIAG_BY_FAMILY")
	require.EqualValues(t, 1, binary.LittleEndian.Uint16(req[6:]), "NLM_F_REQUEST only: an exact lookup, not a dump")
	require.EqualValues(t, 7, binary.LittleEndian.Uint32(req[8:]), "nlmsg_seq")
	require.EqualValues(t, 1, req[16], "AF_UNIX")
	require.EqualValues(t, 0xffffffff, binary.LittleEndian.Uint32(req[20:]), "every state")
	require.EqualValues(t, 3051067119, binary.LittleEndian.Uint32(req[24:]), "udiag_ino")
	require.EqualValues(t, 1, binary.LittleEndian.Uint32(req[28:]), "UDIAG_SHOW_NAME")
	require.EqualValues(t, 0xffffffff, binary.LittleEndian.Uint32(req[32:]), "INET_DIAG_NOCOOKIE")
	require.EqualValues(t, 0xffffffff, binary.LittleEndian.Uint32(req[36:]), "INET_DIAG_NOCOOKIE")
}

// diagReply builds one netlink message: a unix_diag_msg with an optional
// UNIX_DIAG_NAME, or an NLMSG_ERROR.
func diagReply(seq uint32, family, state byte, ino uint32, name []byte, hasName bool) []byte {
	payload := make([]byte, 16)
	payload[0], payload[2] = family, state
	binary.LittleEndian.PutUint32(payload[4:], ino)
	if hasName {
		attr := make([]byte, 4+len(name))
		binary.LittleEndian.PutUint16(attr[0:], uint16(len(attr)))
		copy(attr[4:], name)
		for len(attr)%4 != 0 {
			attr = append(attr, 0)
		}
		payload = append(payload, attr...)
	}
	return nlMessage(seq, 20, payload)
}

func diagError(seq uint32, errno syscall.Errno) []byte {
	payload := make([]byte, 20)
	binary.LittleEndian.PutUint32(payload, uint32(-int32(errno)))
	return nlMessage(seq, 2, payload)
}

// testPort is the netlink port id the replies below are addressed to, as the
// kernel addresses each reply to the requesting socket's port (measured).
const testPort = 3463969

func nlMessage(seq uint32, typ uint16, payload []byte) []byte {
	return nlMessageTo(seq, testPort, typ, payload)
}

func nlMessageTo(seq, port uint32, typ uint16, payload []byte) []byte {
	msg := make([]byte, 16, 16+len(payload))
	binary.LittleEndian.PutUint32(msg[0:], uint32(16+len(payload)))
	binary.LittleEndian.PutUint16(msg[4:], typ)
	binary.LittleEndian.PutUint32(msg[8:], seq)
	binary.LittleEndian.PutUint32(msg[12:], port)
	return append(msg, payload...)
}

func TestParseUnixDiagReply(t *testing.T) {
	const seq, ino = 3, 4242
	for _, tc := range []struct {
		name  string
		reply []byte
		want  unixDiagAnswer
	}{
		{"a listener bound to a path, with its NUL", diagReply(seq, 1, 10, ino, []byte("./d.sock\x00"), true),
			unixDiagAnswer{listening: true, path: "./d.sock"}},
		{"a connected socket bound to a path", diagReply(seq, 1, 1, ino, []byte("/tmp/tmux-1000/default\x00"), true),
			unixDiagAnswer{path: "/tmp/tmux-1000/default"}},
		{"an abstract listener", diagReply(seq, 1, 10, ino, []byte("\x00af-test\x00x"), true),
			unixDiagAnswer{listening: true, path: "@af-test@x", abstract: true}},
		{"an unbound socket has no name", diagReply(seq, 1, 1, ino, nil, false), unixDiagAnswer{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUnixDiagReply(tc.reply, seq, ino, testPort)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}

	_, err := parseUnixDiagReply(diagError(seq, syscall.ENOENT), seq, ino, testPort)
	require.ErrorIs(t, err, errUnixDiagNoSuchSocket, "ENOENT is the kernel's 'no such socket'")
	_, err = parseUnixDiagReply(diagError(seq, syscall.EPERM), seq, ino, testPort)
	require.ErrorIs(t, err, syscall.EPERM)
	require.NotErrorIs(t, err, errUnixDiagNoSuchSocket)

	for name, reply := range map[string][]byte{
		"a reply to another request":   diagReply(seq+1, 1, 10, ino, []byte("/x\x00"), true),
		"an answer for another inode":  diagReply(seq, 1, 10, ino+1, []byte("/x\x00"), true),
		"an answer for another family": diagReply(seq, 2, 10, ino, []byte("/x\x00"), true),
		"an unexpected message type":   nlMessage(seq, 3, make([]byte, 16)),
		"a short unix_diag_msg":        nlMessage(seq, 20, make([]byte, 8)),
		"a length past the buffer":     diagReply(seq, 1, 10, ino, []byte("/x\x00"), true)[:20],
		"nothing at all":               nil,
		// #7543 review R1: a datagram whose nlmsg_len is not 4-aligned and ends
		// the buffer (17 bytes, nlmsg_len 17) used to slice data[20:17] and panic.
		"a 17-byte message whose length is not 4-aligned":      nlMessage(seq, 20, []byte{1}),
		"an unaligned error message ending the buffer":         nlMessage(seq, 2, []byte{0xfe, 0xff, 0xff}),
		"a reply addressed to another port (#7543 review R4)":  nlMessageTo(seq, testPort+1, 20, diagReply(seq, 1, 10, ino, []byte("/x\x00"), true)[16:]),
		"an error addressed to another port (#7543 review R4)": nlMessageTo(seq, testPort+1, 2, diagError(seq, syscall.ENOENT)[16:]),
	} {
		var err error
		require.NotPanics(t, func() { _, err = parseUnixDiagReply(reply, seq, ino, testPort) }, name)
		require.Error(t, err, name)
		require.NotErrorIs(t, err, errUnixDiagNoSuchSocket, name)
	}
}

// The decoder never panics and never answers for a socket it was not asked
// about, whatever bytes arrive (#7543 review R1). `go test` runs the seeds;
// `go test -fuzz=FuzzParseUnixDiagReply ./internal/proctree/` explores.
func FuzzParseUnixDiagReply(f *testing.F) {
	const seq, ino = 3, 4242
	f.Add(diagReply(seq, 1, 10, ino, []byte("./d.sock\x00"), true))
	f.Add(diagReply(seq, 1, 10, ino, []byte("\x00abstract"), true))
	f.Add(diagReply(seq, 1, 1, ino, nil, false))
	f.Add(diagError(seq, syscall.ENOENT))
	f.Add(nlMessage(seq, 20, []byte{1}))
	f.Add(append(diagReply(seq, 1, 10, ino, []byte("/x\x00"), true), nlMessage(seq, 3, nil)...))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		a, err := parseUnixDiagReply(data, seq, ino, testPort)
		if err != nil && a != (unixDiagAnswer{}) {
			t.Fatalf("an error came with an answer: %+v, %v", a, err)
		}
	})
}
