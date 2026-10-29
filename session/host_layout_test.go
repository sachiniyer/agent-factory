package session

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/hostlayout"
)

// Every projection and restore of an instance notes the strings it describes
// about the daemon host, so the response scrub never has to walk sessions
// (internal/hostlayout).
func TestInstanceProjectionAndRestoreNoteHostLayout(t *testing.T) {
	scrubbed := func(s string) bool {
		return !assert.ObjectsAreEqual(s, string(hostlayout.Default.Scrub([]byte(s))))
	}

	inst, err := NewInstance(InstanceOptions{Title: "layout-new", Path: "/srv/layout-new-path/proj", Program: "claude"})
	require.NoError(t, err)
	assert.True(t, scrubbed("/srv/layout-new-path/proj"), "an instance's project path is host layout from construction")

	data := inst.ToInstanceData()
	data.Worktree.WorktreePath = "/srv/layout-wt/worktrees/layout-new"
	data.TmuxName = "af_layout_new_tmux"
	data.Tabs = append(data.Tabs, TabData{TmuxName: "af_layout_new_tab_tmux"})
	// The restore may fail on a fabricated row; the layout is noted before it
	// does, which is the point: a row that exists on disk is covered.
	_, _ = FromInstanceData(data)
	for _, s := range []string{"/srv/layout-wt/worktrees/layout-new", "af_layout_new_tmux", "af_layout_new_tab_tmux"} {
		assert.Truef(t, scrubbed(s), "a restored row's %q is withheld", s)
	}
	assert.False(t, scrubbed("/srv/not-noted-anywhere/x"), "anti-vacuous: an unknown path is left alone")
}

// The response path never takes a session's lock: a scrub completes while an
// instance's write lock is held (the blocking finding on #6014: the old scrub
// waited behind whichever unrelated session held its lock longest).
func TestScrubDoesNotTakeInstanceLocks(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "locked", Path: "/srv/lock-test/proj", Program: "claude"})
	require.NoError(t, err)
	inst.mu.Lock()
	defer inst.mu.Unlock()
	done := make(chan []byte, 1)
	go func() { done <- hostlayout.Default.Scrub([]byte("/srv/lock-test/proj")) }()
	select {
	case out := <-done:
		assert.NotContains(t, string(out), "/srv/lock-test")
	case <-time.After(5 * time.Second):
		t.Fatal("the scrub blocked behind an instance's lock")
	}
}

// The cost of scrubbing a response at about 4000 tracked instances (the shape
// that exposed the per-request cost on #6014): real instances, each projected
// once as the daemon does when it persists or publishes them, then a scrub of a
// 20 KB body. It must stay about one pass over the body, independent of the
// instance count.
func BenchmarkScrubWith4000TrackedInstances(b *testing.B) {
	const n = 4000
	insts := make([]*Instance, 0, n)
	for i := 0; i < n; i++ {
		inst, err := NewInstance(InstanceOptions{
			Title: fmt.Sprintf("bench-%d", i), Path: fmt.Sprintf("/srv/bench/proj%d", i%40), Program: "claude",
		})
		if err != nil {
			b.Fatal(err)
		}
		d := inst.ToInstanceData()
		d.Worktree.WorktreePath = fmt.Sprintf("/srv/bench/.agent-factory/worktrees/bench-%d_%x", i, i*7)
		d.TmuxName = fmt.Sprintf("af_bench-%d", i)
		noteHostLayout(d)
		insts = append(insts, inst)
	}
	body := []byte(strings.Repeat(`{"a":"/srv/bench/proj3/x","b":"nothing here to see at all"} `, 20*1024/58))
	hostlayout.Default.Scrub(body) // compile once, as the first request after a change does
	b.ResetTimer()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		hostlayout.Default.Scrub(body)
	}
	_ = insts
}
