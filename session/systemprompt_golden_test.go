package session

import (
	"os"
	"strings"
	"testing"
)

// goldenUsageReferencePath holds the af usage reference EXACTLY as it read
// before #2172 split it into segments. The file was produced mechanically from
// the pre-split literal (`git show <pre-split>:session/systemprompt.go`), not
// by dumping the current value, so the first run of TestAfUsageReferenceGolden
// proved the split changed nothing rather than merely agreeing with itself.
const goldenUsageReferencePath = "testdata/af_usage_reference.golden"

// TestAfUsageReferenceGolden pins the assembled afUsageReference to its exact
// bytes.
//
// This is the load-bearing lock of the #2172 refactor. afUsageReference used to
// be one literal; it is now a six-part concatenation
//
//	afUsageIntroInside + "\n\n" + afUsageDispatched + "\n\n" + afUsageBody + "\n\n" + afUsageOutroInside + "\n\n" + afUsageOutro
//
// and every joint is a place where a future edit — a trailing space, a "\n\n"
// that becomes "\n" — silently changes the guidance text delivered to EVERY
// agent af launches, on every launch. Nothing else in the tree would notice:
// the plugin-side tests assert that the two renderings SHARE a body, not what
// that body is, so a segment edit that corrupts both renderings identically
// passes all of them.
//
// A failure here is not necessarily a bug. Deliberately editing the af usage
// guidance is normal and expected — but it must be a VISIBLE, reviewed diff of
// the delivered text, which is exactly what this golden forces. When the change
// is intended, regenerate with:
//
//	AF_UPDATE_USAGE_GOLDEN=1 go test ./session/ -run TestAfUsageReferenceGolden
func TestAfUsageReferenceGolden(t *testing.T) {
	if updateUsageGolden() {
		if err := os.WriteFile(goldenUsageReferencePath, []byte(afUsageReference), 0o644); err != nil {
			t.Fatalf("updating %s: %v", goldenUsageReferencePath, err)
		}
		t.Logf("updated %s (%d bytes) — review the diff: this text reaches every agent af launches",
			goldenUsageReferencePath, len(afUsageReference))
		return
	}

	want, err := os.ReadFile(goldenUsageReferencePath)
	if err != nil {
		t.Fatalf("reading %s: %v", goldenUsageReferencePath, err)
	}

	if afUsageReference == string(want) {
		return
	}

	t.Errorf("afUsageReference no longer matches %s.\n"+
		"If you edited the af usage guidance on purpose, re-run with "+
		"AF_UPDATE_USAGE_GOLDEN=1 and review the diff — this text is delivered "+
		"to every agent af launches.\n%s",
		goldenUsageReferencePath, firstDifference(string(want), afUsageReference))
}

// updateUsageGolden reports whether the golden should be rewritten. It is an
// environment variable, not a test flag: the testing package parses flags
// itself and rejects any it does not know, so a custom -update flag has to be
// registered with the flag package to work at all. AF_-prefixed env gating is
// what the rest of this repo's tests already use.
func updateUsageGolden() bool {
	return os.Getenv("AF_UPDATE_USAGE_GOLDEN") == "1"
}

// firstDifference renders the first divergence between want and got with
// surrounding context and the two differing bytes named explicitly. A raw diff
// of a 5.9 KB single-paragraph string is unreadable, and the failures this test
// exists to catch are whitespace — invisible unless quoted.
func firstDifference(want, got string) string {
	n := min(len(want), len(got))
	i := 0
	for i < n && want[i] == got[i] {
		i++
	}

	if i == n {
		return "the texts share a prefix; they differ only in length: " +
			"want " + itoa(len(want)) + " bytes, got " + itoa(len(got)) + " bytes"
	}

	const context = 60
	start := max(0, i-context)
	return "first difference at byte " + itoa(i) + ":\n" +
		"  context: " + quote(want[start:i]) + "\n" +
		"  want:    " + quote(string(want[i])) + "\n" +
		"  got:     " + quote(string(got[i]))
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\n':
			b.WriteString("\\n")
		case '\t':
			b.WriteString("\\t")
		case '"':
			b.WriteString("\\\"")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestAfUsageReferenceJoints names each seam of the concatenation explicitly, so
// a broken joint says WHICH one rather than only that 5.9 KB of text moved. The
// assertions quote the real bytes on both sides of each seam, so they are not a
// restatement of the concatenation expression — that would be circular and
// would pass no matter what the separators became.
func TestAfUsageReferenceJoints(t *testing.T) {
	joints := []struct {
		name string
		want string
	}{
		{
			// intro + "\n\n" + dispatched: a blank line, a new paragraph.
			name: "intro to dispatched",
			want: "the user cannot see.\n\nWhen another instance dispatched you",
		},
		{
			// dispatched + "\n\n" + body: a blank line, a new paragraph.
			name: "dispatched to body",
			want: "no one can help.\n\nCommands print JSON on stdout;",
		},
		{
			// body + "\n\n" + outro: a blank line, a new paragraph.
			name: "body to inside outro",
			want: "no code changes\".\n\nFinishing up: when the user confirms",
		},
		{
			// outro + "\n\n" + tail.
			name: "inside outro to maintenance",
			want: "as the very last step.\n\nMaintenance: af version,",
		},
	}
	for _, j := range joints {
		if !strings.Contains(afUsageReference, j.want) {
			t.Errorf("afUsageReference joint %q is broken; expected to find %q", j.name, j.want)
		}
	}
}

// TestAfPluginUsageReferenceJoints does the same for the plugin rendering. Its
// exact bytes are already pinned indirectly — every committed SKILL.md embeds it
// verbatim, so TestGeneratedPluginsAreCommitted fails on any change — but that
// gate lives in another package and names the artifact, not the seam.
func TestAfPluginUsageReferenceJoints(t *testing.T) {
	joints := []struct {
		name string
		want string
	}{
		{
			name: "plugin intro to body",
			want: `"af daemon status" reports it. Commands print JSON on stdout;`,
		},
		{
			name: "body to plugin outro",
			want: "no code changes\".\n\nFinishing up: the sessions you create",
		},
		{
			name: "plugin outro to maintenance",
			want: "always name the session.\n\nMaintenance: af version,",
		},
	}
	for _, j := range joints {
		if !strings.Contains(AfPluginUsageReference, j.want) {
			t.Errorf("AfPluginUsageReference joint %q is broken; expected to find %q", j.name, j.want)
		}
	}
}

// TestUsageReferencesShareOneBody proves the two renderings are two framings of
// ONE text rather than two texts that happen to overlap: the shared body is
// present in both, whole and unmodified.
func TestUsageReferencesShareOneBody(t *testing.T) {
	if !strings.Contains(afUsageReference, afUsageBody) {
		t.Error("afUsageReference does not contain the shared body verbatim")
	}
	if !strings.Contains(AfPluginUsageReference, afUsageBody) {
		t.Error("AfPluginUsageReference does not contain the shared body verbatim")
	}
	if strings.Contains(AfPluginUsageReference, afUsageIntroInside) {
		t.Error("AfPluginUsageReference carries the inside-af framing; plugin users are not running inside af")
	}
	if strings.Contains(afUsageReference, afUsageIntroPlugin) {
		t.Error("afUsageReference carries the plugin framing; af's own agents ARE running inside af")
	}
	if strings.Contains(AfPluginUsageReference, afUsageDispatched) {
		t.Error("AfPluginUsageReference carries the dispatched-parent escalation guidance; a plugin installer is a caller of af, not a sub-instance a parent dispatched")
	}
}
