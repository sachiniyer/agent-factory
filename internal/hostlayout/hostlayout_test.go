package hostlayout

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScrub(t *testing.T) {
	x := New()
	x.Note("/home/op/proj", "/home/op/proj/.worktrees/s1", "/", "", "ab", "/srv/a&b", "af_sess_one")
	body := []byte(`{"p":"/home/op/proj/.worktrees/s1","q":"/home/op/proj","r":"/srv/a&b/x","t":"af_sess_one","s":"keep /tmp ab"}`)
	got := string(x.Scrub(body))
	assert.NotContains(t, got, "/home/op")
	assert.NotContains(t, got, `a&b`)
	assert.NotContains(t, got, "af_sess_one")
	assert.Contains(t, got, "keep /tmp ab", "short and root entries must not shred ordinary text")
	assert.Equal(t, 4, strings.Count(got, Withheld), "the worktree path is replaced whole, not as project path plus a tail")
}

// A string noted after the replacer was built is covered by the very next scrub:
// invalidation is by change, so what a handler just created is never missed.
func TestNoteInvalidatesTheCompiledReplacer(t *testing.T) {
	x := New()
	x.Note("/home/op/first")
	body := []byte(`/home/op/first and /home/op/second`)
	assert.Contains(t, string(x.Scrub(body)), "/home/op/second", "not yet noted")
	x.Note("/home/op/second")
	assert.NotContains(t, string(x.Scrub(body)), "/home/op", "the next scrub sees the new string")

	n := x.Len()
	x.Note("/home/op/second", "/home/op/second/")
	assert.Equal(t, n, x.Len(), "re-noting a known string adds nothing")
}

func TestScrubOfAnEmptyIndexIsANoOp(t *testing.T) {
	assert.Equal(t, "x /a/b y", string(New().Scrub([]byte("x /a/b y"))))
}

// The scrub costs about one pass over the body however many sessions are
// tracked (the measurement behind #6014's blocking finding: 4017 sessions, ~12k
// needles, was 104 ms / 609 ms / 4.7 s for 2 / 20 / 200 KB with per-needle copies).
func benchIndex(b *testing.B, sessions int) *Index {
	x := New()
	for i := 0; i < sessions; i++ {
		x.Note(
			fmt.Sprintf("/home/op/proj%d", i%40),
			fmt.Sprintf("/home/op/.agent-factory/worktrees/sess-%d_abc%d", i, i*7),
			fmt.Sprintf("af_sess-%d_abc", i),
			fmt.Sprintf("af_sess-%d_tab", i),
		)
	}
	x.Scrub([]byte("warm")) // compile once, as the first request after a change does
	return x
}

func BenchmarkScrub4000Sessions(b *testing.B) {
	for _, kb := range []int{2, 20, 200} {
		x := benchIndex(b, 4000)
		unit := `{"a":"/home/op/proj3/x","b":"nothing here to see at all"} `
		body := []byte(strings.Repeat(unit, kb*1024/len(unit)))
		b.Run(fmt.Sprintf("%dKB", kb), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				x.Scrub(body)
			}
		})
	}
}

// Rebuilding after a change is paid once per change, not per request.
func BenchmarkRebuildAfterOneNew4000Sessions(b *testing.B) {
	x := benchIndex(b, 4000)
	body := []byte("/home/op/proj3/x")
	for i := 0; i < b.N; i++ {
		x.Note(fmt.Sprintf("/home/op/new-%d", i))
		x.Scrub(body)
	}
}
