package session

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseHostPrereqs(t *testing.T) {
	cases := []struct {
		name         string
		out          string
		wantProblems int
		wantWarnings int
		contains     string
	}{
		{name: "all present", out: "git=ok\ntmux=tmux 3.4\ngh=ok\n"},
		{name: "tmux at the floor", out: "git=ok\ntmux=tmux 3.2a\ngh=ok\n"},
		{name: "tmux master build parses", out: "git=ok\ntmux=tmux next-3.5\ngh=ok\n"},
		{name: "unparseable tmux version is tolerated", out: "git=ok\ntmux=tmux master\ngh=ok\n"},
		{name: "git missing", out: "git=missing\ntmux=tmux 3.4\ngh=ok\n", wantProblems: 1, contains: "install git"},
		{name: "tmux missing", out: "git=ok\ntmux=missing\ngh=ok\n", wantProblems: 1, contains: "tmux 3.2 or newer"},
		{name: "tmux too old", out: "git=ok\ntmux=tmux 3.1c\ngh=ok\n", wantProblems: 1, contains: "upgrade tmux"},
		{name: "gh missing only warns", out: "git=ok\ntmux=tmux 3.4\ngh=missing\n", wantWarnings: 1},
		{name: "several problems are all named", out: "git=missing\ntmux=missing\ngh=missing\n", wantProblems: 2, wantWarnings: 1},
		{name: "empty output refuses nothing", out: ""},
		{name: "garbage refuses nothing", out: "Welcome to the host!\nbanner\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := parseHostPrereqs(tc.out)
			assert.Len(t, r.Problems, tc.wantProblems)
			assert.Len(t, r.Warnings, tc.wantWarnings)
			if tc.contains != "" {
				assert.Contains(t, r.Problems[0], tc.contains)
			}
		})
	}
}

type prereqShell struct {
	out   string
	err   error
	calls int
}

func (s *prereqShell) Run(time.Duration, string, io.Reader, bool) ([]byte, error) {
	s.calls++
	return []byte(s.out), s.err
}

func TestCheckHostPrereqs(t *testing.T) {
	sh := &prereqShell{out: "git=missing\ntmux=tmux 3.4\ngh=ok\n"}
	err := (&sandboxWorkspace{shell: sh}).checkHostPrereqs("", time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "install git")
	assert.Equal(t, 1, sh.calls, "one round trip covers every tool")

	sh = &prereqShell{out: "git=ok\ntmux=tmux 3.4\ngh=missing\n"}
	assert.NoError(t, (&sandboxWorkspace{shell: sh}).checkHostPrereqs("", time.Second), "a missing gh warns, never refuses")

	sh = &prereqShell{err: errors.New("ssh: connection reset")}
	assert.NoError(t, (&sandboxWorkspace{shell: sh}).checkHostPrereqs("", time.Second), "a failed probe defers to the provision steps' own errors")
}

func TestHostReadinessCacheStates(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	prev := hostReadinessNow
	t.Cleanup(func() { hostReadinessNow = prev })
	hostReadinessNow = func() time.Time { return now }

	assert.Equal(t, HostReadinessUnprobed, ReadHostReadiness("ssh box").State)

	RecordHostReadiness("ssh box", nil)
	assert.Equal(t, HostReadinessReady, ReadHostReadiness("ssh box").State)

	now = now.Add(hostReadinessFreshFor + time.Minute)
	assert.Equal(t, HostReadinessStale, ReadHostReadiness("ssh box").State, "an old pass is stale, never plain ready")

	RecordHostReadiness("ssh box", errors.New("git is missing"))
	r := ReadHostReadiness("ssh box")
	assert.Equal(t, HostReadinessBlocked, r.State)
	assert.Equal(t, "git is missing", r.Reason)

	now = now.Add(hostReadinessBlockFor + time.Second)
	assert.Equal(t, HostReadinessStale, ReadHostReadiness("ssh box").State, "a blocking verdict stops blocking so the backend can be re-probed")

	RecordHostReadiness("  ", errors.New("ignored"))
	assert.Equal(t, HostReadinessUnprobed, ReadHostReadiness("").State)
}

func TestCheckHostPrereqsRecordsTheVerdict(t *testing.T) {
	key := "ssh record-test-host"
	sh := &prereqShell{out: "git=missing\ntmux=tmux 3.4\ngh=ok\n"}
	_ = (&sandboxWorkspace{shell: sh}).checkHostPrereqs(key, time.Second)
	assert.Equal(t, HostReadinessBlocked, ReadHostReadiness(key).State)

	key2 := "ssh record-test-host-2"
	sh2 := &prereqShell{err: errors.New("ssh: connection refused")}
	_ = (&sandboxWorkspace{shell: sh2}).checkHostPrereqs(key2, time.Second)
	assert.Equal(t, HostReadinessUnprobed, ReadHostReadiness(key2).State, "a probe that never completed says nothing about the host")
}

// Only a complete, positively parsed answer may produce a ready verdict; an
// empty, garbled or truncated one is unknown and must not overwrite a block.
func TestCheckHostPrereqsNeverCachesReadyFromUnreadableOutput(t *testing.T) {
	for name, out := range map[string]string{
		"empty":            "",
		"garbage":          "Welcome to the host!\nbanner\n",
		"truncated":        "git=ok\ntmux=tmux 3.4\n",
		"truncated midway": "git=ok\n",
		"unknown values":   "git=maybe\ntmux=\ngh=what\n",
	} {
		t.Run(name, func(t *testing.T) {
			assert.False(t, parseHostPrereqs(out).Complete)
			key := "ssh unreadable-" + name
			require.NoError(t, (&sandboxWorkspace{shell: &prereqShell{out: out}}).checkHostPrereqs(key, time.Second))
			assert.Equal(t, HostReadinessUnprobed, ReadHostReadiness(key).State)

			RecordHostReadiness(key, errors.New("git is missing"))
			require.NoError(t, (&sandboxWorkspace{shell: &prereqShell{out: out}}).checkHostPrereqs(key, time.Second))
			assert.Equal(t, HostReadinessBlocked, ReadHostReadiness(key).State, "no evidence the host was fixed")
		})
	}
	assert.True(t, parseHostPrereqs("git=ok\ntmux=tmux 3.4\ngh=missing\n").Complete)

	key := "ssh complete-ready"
	require.NoError(t, (&sandboxWorkspace{shell: &prereqShell{out: "git=ok\ntmux=tmux 3.4\ngh=ok\n"}}).checkHostPrereqs(key, time.Second))
	assert.Equal(t, HostReadinessReady, ReadHostReadiness(key).State)
}
