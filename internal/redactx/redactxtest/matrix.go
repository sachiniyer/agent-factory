// Package redactxtest is the shared conformance matrix for the
// normalization stage: one table of (encoding × carrier) rows that every
// secret scrubber must pass. Six leaks in three implementations happened
// because each scrubber was tested against the encodings its author happened
// to think of (#4149). A row added here is a row every consumer inherits —
// adding a transform cannot require editing three test files, or the matrix
// drifts the way the scrubbers did.
//
// A row's Text embeds a plaintext secret under one encoding in one carrier.
// Consumers run only the rows naming them, because the carriers differ:
// credscrub and bugreport scrub log text, agentproto scrubs parsed URLs and
// unstructured text, and only bugreport owns config documents.
package redactxtest

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// Row is one cell of the matrix.
type Row struct {
	Name string
	// Consumers names the implementations this row applies to:
	// "credscrub", "bugreport", "agentproto".
	Consumers []string
	// Via names the surface the row exercises: "log" text, "url" (a bare
	// URI), "json"/"toml" documents, or "text" (agentproto's unstructured
	// pass). Each consumer maps Via to its own entry point.
	Via string
	// Text embeds the plaintext secret in this row's carrier and encoding.
	Text func(secret string) string
	// Keep marks a documented-residue row: the scrub must return the text
	// byte-for-byte, because the encoding is one the stage deliberately does
	// not decode. Keep rows are the residue, named — a normalizer that claims
	// completeness while having a residue is worse than one that names its
	// limits.
	Keep bool
}

// Applies reports whether this row names consumer.
func (r Row) Applies(consumer string) bool {
	for _, c := range r.Consumers {
		if c == consumer {
			return true
		}
	}
	return false
}

var (
	all          = []string{"credscrub", "bugreport", "agentproto"}
	logScrubbers = []string{"credscrub", "bugreport"}
)

// Rows is the conformance table. Every Redact row asserts the plaintext
// secret does not survive the consumer's scrub; every Keep row asserts the
// text passes through untouched.
var Rows = []Row{
	// --- baseline ---
	{Name: "raw", Consumers: logScrubbers, Via: "log",
		Text: func(s string) string { return "note " + s + " end" }},

	// --- percent encoding, parser-proven URI components ---
	{Name: "uri-path-encoded", Consumers: all, Via: "url",
		Text: func(s string) string { return "http://h/" + pctPath(s) }},
	{Name: "uri-path-nested-encoded", Consumers: all, Via: "url",
		Text: func(s string) string { return "http://h/" + pctNested(s) }},
	{Name: "uri-query-value-encoded", Consumers: logScrubbers, Via: "log",
		Text: func(s string) string { return "fetch http://h/?k=" + pct(s) + " ok" }},
	{Name: "uri-query-key-encoded", Consumers: all, Via: "url",
		Text: func(s string) string { return "http://h/?" + pct("access_token") + "=" + pct(s) }},
	{Name: "uri-fragment-key-encoded", Consumers: all, Via: "url",
		Text: func(s string) string { return "http://h/p#access_token=" + pct(s) }},
	{Name: "uri-opaque-key-encoded", Consumers: all, Via: "url",
		Text: func(s string) string { return "mailto:" + pct("access_token="+s) }},
	{Name: "access-token-in-text", Consumers: all, Via: "text",
		Text: func(s string) string { return "err access_token=" + s + " failed" }},

	// --- Go %q transport decoding ---
	{Name: "go-quoted-escapes", Consumers: logScrubbers, Via: "log",
		Text: func(s string) string { return `field "` + hexEscapes(s) + `" tail` }},
	{Name: "go-quoted-nested", Consumers: logScrubbers, Via: "log",
		Text: func(s string) string {
			return `outer "` + hexEscapes(`inner "`+hexEscapes(s)+`"`) + `" tail`
		}},

	// --- ANSI controls ---
	{Name: "ansi-mid-token", Consumers: logScrubbers, Via: "log",
		Text: func(s string) string { return "pre " + ansiSplit(s) + " post" }},
	{Name: "ansi-payload-secret", Consumers: logScrubbers, Via: "log",
		Text: func(s string) string { return "pre \x1b]8;;" + s + "\x1b\\ post" }},

	// --- proven shell commands ---
	{Name: "shell-emitter-quoted-concat", Consumers: logScrubbers, Via: "log",
		Text: func(s string) string {
			return `post-worktree hook ` + strconv.Quote("touch "+shellSplit(s))
		}},
	{Name: "shell-emitter-quoted-continuation", Consumers: logScrubbers, Via: "log",
		Text: func(s string) string {
			return `post-worktree hook ` + strconv.Quote("touch "+shellCont(s))
		}},
	{Name: "shell-emitter-raw-concat", Consumers: logScrubbers, Via: "log",
		Text: func(s string) string {
			return "running post-worktree hook in /tmp/w (output: /tmp/h.log): touch " + shellSplit(s)
		}},
	{Name: "config-shell-concat", Consumers: []string{"bugreport"}, Via: "toml",
		Text: func(s string) string {
			return `post_worktree_commands = ["touch ` + shellSplit(s) + `"]`
		}},

	// --- document scalar decoding ---
	{Name: "json-escaped-scalar", Consumers: []string{"bugreport"}, Via: "json",
		Text: func(s string) string { return `{"data":"` + jsonEscapes(s) + `"}` }},
	{Name: "toml-escaped-scalar", Consumers: []string{"bugreport"}, Via: "toml",
		Text: func(s string) string { return `data = "` + tomlEscapes(s) + `"` }},
	{Name: "toml-comment-credential", Consumers: []string{"bugreport"}, Via: "toml",
		Text: func(s string) string { return `data = "v" # password=` + s }},

	// --- named residue: encodings the stage deliberately does not decode.
	// Each asserts byte-for-byte pass-through. ---
	{Name: "residue-percent-in-prose", Consumers: all, Via: "text", Keep: true,
		Text: func(s string) string { return "rate " + pct(s) + " noted" }},
	{Name: "residue-base64-in-prose", Consumers: all, Via: "text", Keep: true,
		Text: func(s string) string { return "blob " + base64(s) + " ok" }},
	{Name: "residue-shell-punctuation-in-prose", Consumers: logScrubbers, Via: "log", Keep: true,
		Text: func(s string) string { return "it ran " + shellSplit(s) + " plainly" }},
	{Name: "residue-newline-not-joined", Consumers: logScrubbers, Via: "log", Keep: true,
		Text: func(string) string { return "bearer\nT0K3Nx9zw plain" }},
}

// Run drives the matrix against one consumer's scrub function. Via "log"
// text goes to the consumer's log-text scrub; the consumer's driver maps the
// other vias to its own entry points before calling this.
func Run(t *testing.T, consumer, via string, scrub func(string) string, secrets []string) {
	t.Helper()
	for _, row := range Rows {
		if row.Via != via || !row.Applies(consumer) {
			continue
		}
		for _, secret := range secrets {
			t.Run(fmt.Sprintf("%s/%s", row.Name, scrubName(secret)), func(t *testing.T) {
				input := row.Text(secret)
				got := scrub(input)
				if row.Keep {
					if got != input {
						t.Fatalf("%s row %q: residue text rewritten: %q", consumer, row.Name, got)
					}
					return
				}
				if strings.Contains(got, secret) {
					t.Fatalf("%s row %q: secret survived its encoding: %q", consumer, row.Name, got)
				}
			})
		}
	}
}

// scrubName shortens a secret to a printable subtest name.
func scrubName(secret string) string {
	if len(secret) > 24 {
		return secret[:24] + "…"
	}
	return secret
}

// pct percent-encodes every byte of s.
func pct(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteByte('%')
		b.WriteByte(hex[s[i]>>4])
		b.WriteByte(hex[s[i]&0xF])
	}
	return b.String()
}

// pctPath percent-encodes every byte except '/', so an absolute path secret
// keeps its URI path separators.
func pctPath(s string) string {
	var b strings.Builder
	for _, seg := range strings.Split(s, "/") {
		if b.Len() > 0 {
			b.WriteByte('/')
		}
		b.WriteString(pct(seg))
	}
	return b.String()
}

// pctNested encodes once, then escapes the '%' signs again: the carrier must
// be decoded twice before the secret appears.
func pctNested(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteString("%25")
		b.WriteByte(hex[s[i]>>4])
		b.WriteByte(hex[s[i]&0xF])
	}
	return b.String()
}

// hexEscapes renders every byte as a Go \xNN escape — the form %q writes for
// bytes outside the printable set, and a perfectly legal spelling of any
// byte inside one.
func hexEscapes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, `\x%02x`, s[i])
	}
	return b.String()
}

// jsonEscapes renders every byte as a JSON \u00NN escape.
func jsonEscapes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, `\u%04x`, s[i])
	}
	return b.String()
}

// tomlEscapes renders every byte as a TOML \u00NN escape.
func tomlEscapes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, `\u%04X`, s[i])
	}
	return b.String()
}

// ansiSplit drops a complete zero-width control into the middle of the token.
func ansiSplit(s string) string {
	half := len(s) / 2
	return s[:half] + "\x1b[1m" + s[half:]
}

// shellSplit splits the token across a quote boundary so its literal bytes
// only exist as the shell's concatenation.
func shellSplit(s string) string {
	half := len(s) / 2
	return "'" + s[:half] + "''" + s[half:] + "'"
}

// shellCont breaks the token across a line continuation, which the shell
// erases before the word exists.
func shellCont(s string) string {
	half := len(s) / 2
	return s[:half] + "\\\n" + s[half:]
}

const b64alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// base64 is a deliberately tiny encoder: just enough for the residue row,
// which asserts the stage does NOT see through base64 armor.
func base64(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i += 3 {
		var n uint32
		rem := len(s) - i
		if rem > 3 {
			rem = 3
		}
		for j := 0; j < rem; j++ {
			n = n<<8 | uint32(s[i+j])
		}
		n <<= 8 * uint(3-rem)
		for j := 0; j < 4; j++ {
			if j < rem+1 {
				b.WriteByte(b64alphabet[n>>18&0x3F])
			} else {
				b.WriteByte('=')
			}
			n <<= 6
		}
	}
	return b.String()
}
