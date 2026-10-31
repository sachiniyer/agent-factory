package app

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// #6423: materializing a snapshot restores every session of the project, and each
// restore spawns tmux subprocesses (has-session, list-panes, display-message). Done
// one session at a time before the first frame, the launch cost grew with the
// project: 150 tmux calls and ~2.4 s for 50 sessions. These tests pin the property
// that the builds overlap (bounded) while the store still receives the sessions in
// snapshot order and a failed build is still skipped.

// snapshotRecords returns n records titled s00, s01, ... and a title→instance
// table built on the test goroutine (instance construction uses require, which
// must not be called from the builder goroutines).
func snapshotRecords(t *testing.T, n int) ([]session.InstanceData, map[string]*session.Instance) {
	t.Helper()
	data := make([]session.InstanceData, n)
	insts := make(map[string]*session.Instance, n)
	for i := range data {
		title := fmt.Sprintf("s%02d", i)
		data[i] = session.InstanceData{Title: title}
		insts[title] = newSnapshotTestInstance(t, title)
	}
	return data, insts
}

func TestMaterializeSnapshot_BuildsConcurrentlyInSnapshotOrder(t *testing.T) {
	h := newTestHome(t)
	h.snapshotBuildWorkers = 0 // the production default: concurrent
	data, insts := snapshotRecords(t, 24)

	var inFlight, maxInFlight atomic.Int32
	var overlapped sync.Once
	gate := make(chan struct{})
	var gaveUp atomic.Bool
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		if cur >= 2 {
			overlapped.Do(func() { close(gate) })
		}
		// Hold this build until another one is in flight. A builder that is only
		// ever called one at a time never sees a second, so it gives up after a
		// bounded wait (once, not per call) and the assertions below name why.
		if !gaveUp.Load() {
			select {
			case <-gate:
			case <-time.After(2 * time.Second):
				gaveUp.Store(true)
			}
		}
		return insts[d.Title], nil
	}))

	h.materializeSnapshot(data)

	require.GreaterOrEqual(t, int(maxInFlight.Load()), 2,
		"snapshot builds ran one at a time: each restore's tmux calls serialize the launch behind the session count (#6423)")
	require.LessOrEqual(t, int(maxInFlight.Load()), 32,
		"the build fan-out must stay bounded so a large project cannot flood tmux with subprocesses")

	got := h.store.GetInstances()
	require.Len(t, got, len(data))
	for i, inst := range got {
		require.Equal(t, data[i].Title, inst.Title, "sessions must reach the store in snapshot order, not completion order")
	}
}

func TestMaterializeSnapshot_SkipsAFailedBuildAndKeepsTheRest(t *testing.T) {
	h := newTestHome(t)
	h.snapshotBuildWorkers = 0 // the production default: concurrent
	data, insts := snapshotRecords(t, 12)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		if d.Title == "s03" {
			return nil, errors.New("tmux session destroyed externally")
		}
		return insts[d.Title], nil
	}))

	h.materializeSnapshot(data)

	require.Nil(t, findSidebarInstance(h, "s03"), "an unrestorable record is skipped, not added")
	got := h.store.GetInstances()
	require.Len(t, got, len(data)-1)
	want := make([]string, 0, len(data)-1)
	for _, d := range data {
		if d.Title != "s03" {
			want = append(want, d.Title)
		}
	}
	titles := make([]string, len(got))
	for i, inst := range got {
		titles[i] = inst.Title
	}
	require.Equal(t, want, titles, "the remaining sessions keep snapshot order around the skipped one")
}

// #6423: the batch primes one tmux listing before the restores and releases it
// after the last one, so the restores' existence/generation/pane-row probes answer
// from it instead of asking tmux per session.
func TestMaterializeSnapshot_PrimesTheTmuxSnapshotAroundTheBatchOnce(t *testing.T) {
	h := newTestHome(t)
	data, insts := snapshotRecords(t, 6)

	var mu sync.Mutex
	var events []string
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	h.primeSessionSnapshot = func() func() {
		record("prime")
		return func() { record("release") }
	}
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		record("build")
		return insts[d.Title], nil
	}))

	h.materializeSnapshot(data)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, len(data)+2)
	require.Equal(t, "prime", events[0], "the listing must be primed before any restore asks tmux")
	require.Equal(t, "release", events[len(events)-1], "and released once the batch is done")
	for _, e := range events[1 : len(events)-1] {
		require.Equal(t, "build", e, "prime and release each happen exactly once per batch")
	}
}

// #6446: every restore of a batch shares ONE RestoreContext, so the project's
// repo and config resolution runs once per batch rather than once per session.
func TestMaterializeSnapshot_ShareOneRestoreContextAcrossTheBatch(t *testing.T) {
	h := newTestHome(t)
	h.snapshotBuildWorkers = 0 // the production default: concurrent
	data, insts := snapshotRecords(t, 12)

	var mu sync.Mutex
	seen := map[*session.RestoreContext]int{}
	t.Cleanup(SetInstanceBuilderWithContextForTest(func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		mu.Lock()
		seen[rc]++
		mu.Unlock()
		return insts[d.Title], nil
	}))

	h.materializeSnapshot(data)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, 1, "all restores in a batch must share one context")
	for rc, n := range seen {
		require.NotNil(t, rc, "a batch must pass a context, not nil")
		require.Equal(t, len(data), n)
	}
}
