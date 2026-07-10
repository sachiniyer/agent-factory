package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHookPTYIngestED2SnapshotReset exercises the snapshot-reset semantics
// added for #810: ED2 (\x1b[2J) in the attach_cmd stream marks the start of
// a fresh snapshot, so everything up to and including the last ED2 is
// dropped instead of concatenated with stale frames.
func TestHookPTYIngestED2SnapshotReset(t *testing.T) {
	t.Run("two snapshots in one write keeps only the latest", func(t *testing.T) {
		hp := &hookPTY{}
		hp.ingest([]byte("\x1b[2J\x1b[Hframe one\x1b[2J\x1b[Hframe two"))
		assert.Equal(t, "\x1b[Hframe two", string(hp.buf))
	})

	t.Run("snapshots across separate writes keep only the latest", func(t *testing.T) {
		hp := &hookPTY{}
		hp.ingest([]byte("\x1b[2J\x1b[Hfirst frame"))
		hp.ingest([]byte("\x1b[2J\x1b[Hsecond frame"))
		assert.Equal(t, "\x1b[Hsecond frame", string(hp.buf))
	})

	t.Run("ED2 split across read boundaries is still detected", func(t *testing.T) {
		hp := &hookPTY{}
		hp.ingest([]byte("stale frame\x1b["))
		hp.ingest([]byte("2Jfresh frame"))
		assert.Equal(t, "fresh frame", string(hp.buf))
	})

	t.Run("stream without ED2 accumulates", func(t *testing.T) {
		hp := &hookPTY{}
		hp.ingest([]byte("hello "))
		hp.ingest([]byte("world"))
		assert.Equal(t, "hello world", string(hp.buf))
	})

	t.Run("buffer without ED2 is still capped at 64KiB keeping the tail", func(t *testing.T) {
		hp := &hookPTY{}
		hp.ingest(bytes64k('a'))
		hp.ingest([]byte("tail"))
		require.Len(t, hp.buf, 64*1024)
		assert.True(t, strings.HasSuffix(string(hp.buf), "tail"))
	})
}

func bytes64k(c byte) []byte {
	b := make([]byte, 64*1024)
	for i := range b {
		b[i] = c
	}
	return b
}

// TestSanitizeHookPreview exercises the read-side sanitizer added for #810:
// every escape/control sequence except SGR colors (and \n/\t) must be
// stripped before the preview pane embeds the string in its frame.
func TestSanitizeHookPreview(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "clear screen and cursor home",
			in:   "\x1b[2J\x1b[Hhello",
			want: "hello",
		},
		{
			name: "alt screen enter and leave",
			in:   "\x1b[?1049hhi\x1b[?1049l",
			want: "hi",
		},
		{
			name: "cursor hide and show",
			in:   "\x1b[?25lworking\x1b[?25h",
			want: "working",
		},
		{
			name: "cursor positioning and movement",
			in:   "\x1b[10;5Hup\x1b[Adown\x1b[2Bleft\x1b[3Dright\x1b[4C",
			want: "updownleftright",
		},
		{
			name: "scroll region",
			in:   "\x1b[r\x1b[1;24rtext",
			want: "text",
		},
		{
			name: "erase line variants",
			in:   "a\x1b[Kb\x1b[2Kc",
			want: "abc",
		},
		{
			name: "OSC title BEL-terminated",
			in:   "\x1b]0;window title\x07text",
			want: "text",
		},
		{
			name: "OSC 8 hyperlink ST-terminated",
			in:   "\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\",
			want: "link",
		},
		{
			name: "single-char escapes and charset designation",
			in:   "\x1b=a\x1b>b\x1b(Bc\x1bMd",
			want: "abcd",
		},
		{
			name: "device status query",
			in:   "before\x1b[6nafter",
			want: "beforeafter",
		},
		{
			name: "basic SGR colors kept",
			in:   "\x1b[31mred\x1b[0m plain \x1b[1;4mbold\x1b[m",
			want: "\x1b[31mred\x1b[0m plain \x1b[1;4mbold\x1b[m",
		},
		{
			name: "256-color and truecolor SGR kept",
			in:   "\x1b[38;5;196mx\x1b[48;2;0;128;255my\x1b[0m",
			want: "\x1b[38;5;196mx\x1b[48;2;0;128;255my\x1b[0m",
		},
		{
			name: "colon subparameter SGR kept",
			in:   "\x1b[4:3mcurly\x1b[0m",
			want: "\x1b[4:3mcurly\x1b[0m",
		},
		{
			name: "private-prefix m-final sequence is not SGR",
			in:   "\x1b[?4mtext",
			want: "text",
		},
		{
			name: "SGR mixed with control sequences",
			in:   "\x1b[2J\x1b[H\x1b[32mgreen\x1b[0m\x1b[5;1Hmore",
			want: "\x1b[32mgreen\x1b[0mmore",
		},
		{
			name: "bare carriage returns dropped",
			in:   "progress 10%\rprogress 99%\rdone",
			want: "progress 10%progress 99%done",
		},
		{
			name: "CRLF collapses to LF",
			in:   "line1\r\nline2\r\n",
			want: "line1\nline2\n",
		},
		{
			name: "C0 controls dropped except newline and tab",
			in:   "a\x07b\x08c\td\ne\x0c",
			want: "abc\td\ne",
		},
		{
			name: "incomplete trailing CSI dropped",
			in:   "text\x1b[3",
			want: "text",
		},
		{
			name: "lone trailing ESC dropped",
			in:   "text\x1b",
			want: "text",
		},
		{
			name: "unterminated OSC dropped",
			in:   "text\x1b]0;never terminated",
			want: "text",
		},
		{
			name: "UTF-8 text intact",
			in:   "\x1b[2J╭─ box ❯ prompt › arrows ╰",
			want: "╭─ box ❯ prompt › arrows ╰",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeHookPreview(tc.in)
			assert.Equal(t, tc.want, got)
			// Invariant for every case: no ESC byte survives except as the
			// start of an SGR sequence, and no stray \r or other C0 controls
			// besides \n and \t remain.
			assertOnlySGREscapes(t, got)
		})
	}
}

// TestSanitizeHookPreviewKeepsReadySignals pins the contract that
// task.WaitForReady depends on: the per-agent ready glyphs that
// isReadyContent matches (codex "›", claude "❯", aider "\n> ", gemini "╰",
// amp's mode-labeled input frame) must survive sanitization of a realistic raw
// remote capture.
func TestSanitizeHookPreviewKeepsReadySignals(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "codex prompt glyph",
			raw:  "\x1b[2J\x1b[H\x1b[?25l\x1b[1mCodex\x1b[0m\r\n\r\n› ",
			want: "›",
		},
		{
			name: "claude prompt glyph",
			raw:  "\x1b[?1049h\x1b[2J\x1b[HWelcome to Claude Code\r\n❯ \x1b[?25h",
			want: "❯",
		},
		{
			name: "aider line-start prompt",
			raw:  "\x1b[2JAider v0.1\r\n> ",
			want: "\n> ",
		},
		{
			name: "gemini frame corner",
			raw:  "\x1b[2J╭───╮\r\n╰───╯\r\n",
			want: "╰",
		},
		{
			name: "amp input frame",
			raw:  "\x1b[2JWelcome to Amp\r\n╭──────── medium ────────╮\r\n│ > \x1b[?25h",
			want: "╭",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeHookPreview(tc.raw)
			assert.Contains(t, got, tc.want)
		})
	}
}

// TestHookBackendPreviewSanitizesBuffer covers the full read path: a raw
// buffer seeded into the backend comes back sanitized from Preview and
// PreviewFullHistory.
func TestHookBackendPreviewSanitizesBuffer(t *testing.T) {
	b := &HookBackend{}
	i := &Instance{Title: "remote-test"}
	b.SetPreviewBufferForTest("remote-test", []byte("\x1b[?1049h\x1b[2J\x1b[H\x1b[31mred\x1b[0m\r\nplain\x1b[5;1H"))

	for name, fn := range map[string]func(*Instance) (string, error){
		"Preview":            b.Preview,
		"PreviewFullHistory": b.PreviewFullHistory,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := fn(i)
			require.NoError(t, err)
			assert.Equal(t, "\x1b[31mred\x1b[0m\nplain", got)
			assertOnlySGREscapes(t, got)
		})
	}
}

// assertOnlySGREscapes fails if s contains any ESC byte that does not start
// a well-formed SGR sequence (\x1b[<digits/;/:>m), any carriage return, or
// any other C0 control besides \n and \t.
func assertOnlySGREscapes(t *testing.T, s string) {
	t.Helper()
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\x1b' {
			rest := s[i:]
			end := strings.IndexByte(rest, 'm')
			require.True(t, end >= 2 && isSGRSequence(rest[:end+1]),
				"non-SGR escape at byte %d: %q", i, rest)
			i += end
			continue
		}
		if c == '\n' || c == '\t' {
			continue
		}
		require.False(t, c < 0x20 || c == 0x7f, "raw control byte %q at %d in %q", c, i, s)
	}
}
