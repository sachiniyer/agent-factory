package proctree

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// fdPathFromVnodeFdInfo's contract is exercised here on every platform: the
// darwin backend hands it raw proc_pidfdinfo buffers, and a buffer that is
// not exactly a plausible absolute path must read as "no path", never as a
// truncated or malformed one.
func TestFdPathFromVnodeFdInfo(t *testing.T) {
	makeBuf := func(path string) []byte {
		buf := make([]byte, vnodeFdInfoWithPathSize)
		copy(buf[vnodeFdInfoPathOffset:], path)
		return buf
	}

	t.Run("valid absolute path", func(t *testing.T) {
		got, ok := fdPathFromVnodeFdInfo(makeBuf("/home/u/.codex/sessions/rollout-abc.jsonl"))
		require.True(t, ok)
		require.Equal(t, "/home/u/.codex/sessions/rollout-abc.jsonl", got)
	})

	t.Run("nul terminated inside the buffer", func(t *testing.T) {
		buf := makeBuf("/a/b.jsonl")
		buf[vnodeFdInfoPathOffset+len("/a/b.jsonl")] = 0
		for i := vnodeFdInfoPathOffset + len("/a/b.jsonl") + 1; i < len(buf); i++ {
			buf[i] = 'x'
		}
		got, ok := fdPathFromVnodeFdInfo(buf)
		require.True(t, ok)
		require.Equal(t, "/a/b.jsonl", got)
	})

	t.Run("wrong size is not a path", func(t *testing.T) {
		_, ok := fdPathFromVnodeFdInfo(make([]byte, vnodeFdInfoWithPathSize-1))
		require.False(t, ok)
		_, ok = fdPathFromVnodeFdInfo(make([]byte, vnodeFdInfoWithPathSize+1))
		require.False(t, ok)
	})

	t.Run("empty tail is not a path", func(t *testing.T) {
		_, ok := fdPathFromVnodeFdInfo(make([]byte, vnodeFdInfoWithPathSize))
		require.False(t, ok)
	})

	t.Run("relative tail is not a path", func(t *testing.T) {
		_, ok := fdPathFromVnodeFdInfo(makeBuf("tmp/rollout.jsonl"))
		require.False(t, ok)
	})

	t.Run("control bytes are not a path", func(t *testing.T) {
		_, ok := fdPathFromVnodeFdInfo(makeBuf("/a/\x07b.jsonl"))
		require.False(t, ok)
	})
}
