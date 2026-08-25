package config

import (
	"strings"
	"testing"
)

// TestSetTOMLScalar_PreservesCommentAfterMultilineDelimiter is the #3455
// regression, stated at the level of the promise `af config set` makes in its
// own help text: it "preserves unrelated comments, blank lines, and key
// ordering".
//
// A trailing comment on a multiline string's CLOSING line (`""" # note`) is
// valid TOML and was silently dropped, because splitTrailingComment flipped its
// quote state once per `"` — three quotes left it reading as INSIDE a string, so
// the '#' after them never registered as a comment. Both delimiter forms are
// affected, and so is a closing line that carries content before the delimiter.
func TestSetTOMLScalar_PreservesCommentAfterMultilineDelimiter(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "basic multiline, delimiter alone on the closing line",
			content: "on_archive_command = \"\"\"\necho hi\n\"\"\" # keep me\nbranch_prefix = \"x/\"\n",
			want:    "on_archive_command = \"new\"\n# keep me\nbranch_prefix = \"x/\"\n",
		},
		{
			name:    "literal multiline, delimiter alone on the closing line",
			content: "on_archive_command = '''\necho hi\n''' # keep me\nbranch_prefix = \"x/\"\n",
			want:    "on_archive_command = \"new\"\n# keep me\nbranch_prefix = \"x/\"\n",
		},
		{
			name:    "basic multiline, content before the closing delimiter",
			content: "on_archive_command = \"\"\"\necho hi\nlast\"\"\" # keep me\n",
			want:    "on_archive_command = \"new\"\n# keep me\n",
		},
		{
			name:    "literal multiline, content before the closing delimiter",
			content: "on_archive_command = '''\necho hi\nlast''' # keep me\n",
			want:    "on_archive_command = \"new\"\n# keep me\n",
		},
		{
			name:    "indented closing line keeps its indent",
			content: "on_archive_command = \"\"\"\necho hi\n  \"\"\"   # keep me\n",
			want:    "on_archive_command = \"new\"\n  # keep me\n",
		},
		{
			// The '#' here is STRING CONTENT, not a comment: the opening
			// delimiter has already opened the string by the time it is read.
			// Preserving it as a standalone comment would invent one.
			name:    "a hash on the OPENING line is string content, not a comment",
			content: "on_archive_command = \"\"\" # not a comment\necho hi\n\"\"\"\n",
			want:    "on_archive_command = \"new\"\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := setTOMLScalar(tt.content, "", "on_archive_command", `"new"`); got != tt.want {
				t.Errorf("setTOMLScalar:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// TestSetTOMLScalar_PreservesCommentAfterMultilineArrayElement covers the same
// defect reached through an ARRAY whose element is a multiline string. The
// string stays open across lines there too, so the element's own trailing
// comment — and every later element's — was read as string content.
func TestSetTOMLScalar_PreservesCommentAfterMultilineArrayElement(t *testing.T) {
	content := "session_env_passthrough = [\n  \"\"\"\nA\n\"\"\", # keep me\n  \"B\", # and me\n]\n"
	// Each preserved comment keeps the indent of the LINE IT SAT ON: the closing
	// delimiter here is at column 0, the "B" element is indented two.
	want := "session_env_passthrough = [\"Z\"]\n# keep me\n  # and me\n"
	if got := setTOMLScalar(content, "", "session_env_passthrough", `["Z"]`); got != want {
		t.Errorf("setTOMLScalar:\n got %q\nwant %q", got, want)
	}
}

// TestScanTrailingComment is the scanner's own contract, table-driven. The
// `open` column is the delimiter still open when the line STARTS — "" for a line
// that opens whatever it contains (a single-line value, an inline table), and a
// triple quote for the closing line of a multiline assignment.
//
// The single-quote and escaped-quote rows are non-regression, not new coverage:
// they are the behaviour the per-quote toggle got right, and the rewrite has to
// keep getting right.
func TestScanTrailingComment(t *testing.T) {
	for _, tt := range []struct {
		name        string
		rest        string
		open        string
		wantValue   string
		wantComment string
		wantOpen    string
	}{
		// #3455 — the reported defect, both delimiter forms.
		{name: "basic closing delimiter then comment", rest: `""" # c`, open: `"""`, wantValue: `"""`, wantComment: " # c"},
		{name: "literal closing delimiter then comment", rest: `''' # c`, open: `'''`, wantValue: `'''`, wantComment: " # c"},
		{name: "content then basic closing delimiter", rest: `last""" # c`, open: `"""`, wantValue: `last"""`, wantComment: " # c"},
		{name: "content then literal closing delimiter", rest: `last''' # c`, open: `'''`, wantValue: `last'''`, wantComment: " # c"},
		{name: "hash before the close is string content", rest: `a # b""" # c`, open: `"""`, wantValue: `a # b"""`, wantComment: " # c"},
		{name: "line stays inside the multiline string", rest: `a # b`, open: `"""`, wantValue: `a # b`, wantOpen: `"""`},
		{name: "opening delimiter leaves the string open", rest: `""" # content`, wantValue: `""" # content`, wantOpen: `"""`},
		{name: "balanced multiline on one line", rest: `"""a""" # c`, wantValue: `"""a"""`, wantComment: " # c"},
		{name: "balanced literal multiline on one line", rest: `'''a''' # c`, wantValue: `'''a'''`, wantComment: " # c"},
		{name: "content ending in a quote absorbs the run", rest: `a"""" # c`, open: `"""`, wantValue: `a""""`, wantComment: " # c"},

		// Non-regression: single quotes, escapes, and no-comment lines.
		{name: "hash inside a basic string", rest: `"a # b" # c`, wantValue: `"a # b"`, wantComment: " # c"},
		{name: "hash inside a literal string", rest: `'a # b' # c`, wantValue: `'a # b'`, wantComment: " # c"},
		{name: "escaped quote does not close a basic string", rest: `"a\" # b" # c`, wantValue: `"a\" # b"`, wantComment: " # c"},
		{name: "escaped backslash before the close", rest: `"a\\" # c`, wantValue: `"a\\"`, wantComment: " # c"},
		{name: "a backslash is literal inside a literal string", rest: `'a\' # c`, wantValue: `'a\'`, wantComment: " # c"},
		{name: "no comment at all", rest: `"plain"`, wantValue: `"plain"`},
		{name: "comment only", rest: `# c`, wantValue: "", wantComment: "# c"},
		{name: "tabs before the hash are part of the comment", rest: "1\t\t# c", wantValue: "1", wantComment: "\t\t# c"},
		{name: "unterminated basic string leaves it open", rest: `"a # b`, wantValue: `"a # b`, wantOpen: `"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			value, comment, open := scanTrailingComment(tt.rest, tt.open)
			if value != tt.wantValue || comment != tt.wantComment || open != tt.wantOpen {
				t.Errorf("scanTrailingComment(%q, %q):\n got (%q, %q, %q)\nwant (%q, %q, %q)",
					tt.rest, tt.open, value, comment, open, tt.wantValue, tt.wantComment, tt.wantOpen)
			}
			// value+comment must reconstitute the input byte-for-byte whenever
			// the line ends outside a string; that is what lets the caller
			// reattach the comment without reformatting the operator's file.
			if open == "" && value+comment != tt.rest {
				t.Errorf("value+comment = %q, want the input %q back", value+comment, tt.rest)
			}
		})
	}
}

// TestScanTrailingComment_ReconstitutesEveryLine sweeps every string up to
// length 5 over the alphabet that actually decides this scanner's branches —
// quote, apostrophe, hash, backslash, an ordinary byte and a space — and pins
// the two invariants the callers depend on, in every resume state.
//
// Reconstitution is the load-bearing one: replaceTOMLAssignmentValue rebuilds a
// line as prefix + encoded + value-tail + comment, so a scanner that dropped or
// duplicated a byte would corrupt an operator's config rather than merely
// mis-place a comment.
func TestScanTrailingComment_ReconstitutesEveryLine(t *testing.T) {
	alphabet := []byte{'"', '\'', '#', '\\', 'a', ' '}
	opens := []string{"", `"`, `'`, tomlMultilineBasic, tomlMultilineLiteral}

	var check func(prefix []byte)
	check = func(prefix []byte) {
		if len(prefix) > 0 {
			line := string(prefix)
			for _, open := range opens {
				value, comment, _ := scanTrailingComment(line, open)
				if value+comment != line {
					t.Fatalf("scanTrailingComment(%q, %q) = (%q, %q): the halves do not reconstitute the line", line, open, value, comment)
				}
				if comment != "" && comment[len(comment)-len(strings.TrimLeft(comment, " \t")):][0] != '#' {
					t.Fatalf("scanTrailingComment(%q, %q) reported a comment %q that does not begin with '#'", line, open, comment)
				}
			}
		}
		if len(prefix) == 5 {
			return
		}
		for _, c := range alphabet {
			check(append(prefix, c))
		}
	}
	check(nil)
}

// TestDeleteTOMLScalar_PreservesCommentAfterMultilineDelimiter covers the OTHER
// caller of the same scanner. `af config unset` runs deleteTOMLScalar, which
// reuses preservedTOMLAssignmentComments to keep an operator's notes when the
// assignment around them goes away — so the dropped comment was reachable from
// unset as well as set, and both forms are pinned here.
func TestDeleteTOMLScalar_PreservesCommentAfterMultilineDelimiter(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "basic multiline",
			content: "on_archive_command = \"\"\"\necho hi\n\"\"\" # keep me\nbranch_prefix = \"x/\"\n",
			want:    "# keep me\nbranch_prefix = \"x/\"\n",
		},
		{
			name:    "literal multiline",
			content: "on_archive_command = '''\necho hi\n''' # keep me\nbranch_prefix = \"x/\"\n",
			want:    "# keep me\nbranch_prefix = \"x/\"\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := deleteTOMLScalar(tt.content, "", "on_archive_command")
			if !ok {
				t.Fatalf("deleteTOMLScalar did not find the key")
			}
			if got != tt.want {
				t.Errorf("deleteTOMLScalar:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}
