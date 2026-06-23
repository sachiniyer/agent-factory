package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// sgrPrefixRe matches a well-formed SGR sequence (ESC [ <numeric params> m)
// at the start of a string. Used to verify that any ESC byte reaching the
// rendered pane belongs to a color sequence and nothing else.
var sgrPrefixRe = regexp.MustCompile(`^\x1b\[[0-9;:]*m`)

// TestPreviewRemoteHookStripsControlSequences is the #810 regression test:
// a remote (hook-backend) preview whose raw stream contains clear-screen,
// cursor-positioning, and alt-screen sequences must render through the
// preview pane without any raw control bytes other than SGR colors. Before
// the fix, the ESC[2J/ESC[H bytes in the pane's String() output were
// executed by the real terminal on flush, corrupting the whole TUI.
func TestPreviewRemoteHookStripsControlSequences(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	hook := &session.HookBackend{}
	restore := session.SetBackendFactoryForTest(
		func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
			return hook, nil
		})
	defer restore()

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   "remote-810",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	instance.SetStartedForTest(true)

	// A realistic capture-loop frame: alt-screen, ED2 + cursor home, hidden
	// cursor, colored text, CRLF line endings, and a final cursor move.
	raw := "\x1b[?1049h\x1b[2J\x1b[H\x1b[?25l\x1b[31mhello\x1b[0m\r\nworld\r\n\x1b[5;1H"
	hook.SetPreviewBufferForTest("remote-810", []byte(raw))

	pane := NewTabPane()
	pane.SetSize(80, 24)
	require.NoError(t, pane.UpdateContent(instance, 0))
	out := pane.String()

	require.Contains(t, out, "hello")
	require.Contains(t, out, "world")

	// Every ESC byte in the rendered pane must start an SGR sequence; no
	// erase/cursor/mode sequences and no raw \r may survive.
	for i := 0; i < len(out); i++ {
		if out[i] == '\x1b' {
			m := sgrPrefixRe.FindString(out[i:])
			require.NotEmpty(t, m, "non-SGR escape sequence at byte %d: %q", i, out[i:min(i+16, len(out))])
			i += len(m) - 1
		}
	}
	require.NotContains(t, out, "\r")
	require.NotContains(t, out, "\x1b[2J")
	require.NotContains(t, out, "\x1b[H")
	require.False(t, strings.Contains(out, "\x1b[?"), "private-mode sequence survived into pane output")
}
