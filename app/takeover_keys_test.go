package app

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// fakeTmux interprets the takeover's tmux command sequence with the semantics the
// routing relies on, so the effective detach key of every client can be asserted
// at each step: bindings and options are per session/table, a client attached to
// session G consults G's key-table, and the prefix is checked BEFORE the table
// (server-client.c: "prefix always takes precedence").
type fakeTmux struct {
	sessions map[string]*fakeSession
	tables   map[string]map[string]string // table -> key -> command
}

type fakeSession struct {
	opts    map[string]string
	grouped string
}

func newFakeTmux(shared ...string) *fakeTmux {
	f := &fakeTmux{sessions: map[string]*fakeSession{}, tables: map[string]map[string]string{}}
	for _, s := range shared {
		f.sessions[s] = &fakeSession{opts: map[string]string{"prefix": "C-b", "key-table": "root"}}
	}
	return f
}

// run applies one `;`-separated command group.
func (f *fakeTmux) run(t *testing.T, args []string) {
	t.Helper()
	for _, c := range splitTmuxSequence(args) {
		switch c[0] {
		case "new-session":
			var group, of string
			for i := 1; i < len(c)-1; i++ {
				switch c[i] {
				case "-t":
					of = c[i+1]
				case "-s":
					group = c[i+1]
				}
			}
			require.Contains(t, f.sessions, of, "a grouped session needs the shared one to exist")
			f.sessions[group] = &fakeSession{grouped: of, opts: map[string]string{"prefix": "C-b", "key-table": "root"}}
		case "set-option":
			require.Equal(t, "-t", c[1])
			sess, ok := f.sessions[c[2]]
			require.True(t, ok, "set-option targets an existing session: %v", c)
			sess.opts[c[3]] = c[4]
		case "bind-key":
			require.Equal(t, "-T", c[1], "never a root/-n binding: %v", c)
			if f.tables[c[2]] == nil {
				f.tables[c[2]] = map[string]string{}
			}
			f.tables[c[2]][c[3]] = strings.Join(c[4:], " ")
		case "unbind-key":
			require.Equal(t, []string{"-a", "-T"}, c[1:3])
			delete(f.tables, c[3])
		case "kill-session":
			delete(f.sessions, c[2])
		case "attach-session", "display-message":
		default:
			t.Fatalf("unmodelled tmux command %v", c)
		}
	}
}

func splitTmuxSequence(args []string) [][]string {
	var out [][]string
	var cur []string
	for _, a := range args {
		if a == ";" {
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, a)
	}
	return append(out, cur)
}

// detachKeys is the set of keys that detach a client attached to session g.
func (f *fakeTmux) detachKeys(g string) []string {
	s, ok := f.sessions[g]
	if !ok {
		return nil
	}
	var keys []string
	for k, cmd := range f.tables[s.opts["key-table"]] {
		if cmd == "detach-client" && k != s.opts["prefix"] && k != s.opts["prefix2"] {
			keys = append(keys, k)
		}
	}
	return keys
}

func takeoverSupportedKeys() []string {
	var out []string
	for c := 'a'; c <= 'z'; c++ {
		out = append(out, fmt.Sprintf("ctrl-%c", c))
	}
	return append(out, `ctrl-\`, "ctrl-]", "ctrl-^", "ctrl-_")
}

// #6074/#6075: the matrix. Every supported detach_keys value (ctrl-b included),
// with one and several clients on one login session, in every join and leave
// order: each live client's effective detach key is exactly the configured one at
// every step, and a client leaving never changes another's.
func TestTakeoverKeyRoutingMatrix(t *testing.T) {
	orders := map[string][]string{
		"single":        {"+A", "-A"},
		"fifo":          {"+A", "+B", "-A", "-B"},
		"lifo":          {"+A", "+B", "-B", "-A"},
		"rejoin":        {"+A", "+B", "-A", "+C", "-B", "-C"},
		"leave-first":   {"+A", "-A", "+B", "-B"},
		"three-overlap": {"+A", "+B", "+C", "-B", "-A", "-C"},
	}
	for _, spec := range takeoverSupportedKeys() {
		b, err := config.ParseDetachKey(spec)
		require.NoError(t, err, spec)
		want, ok := tmux.DetachKeyTmuxName(b)
		for name, steps := range orders {
			t.Run(spec+"/"+name, func(t *testing.T) {
				f := newFakeTmux("login")
				live := map[string]takeoverRoute{}
				for _, step := range steps {
					who := step[1:]
					if step[0] == '+' {
						r := newTakeoverRoute("login", b, tmux.DetachKeyDisplay)
						f.run(t, r.attachArgs())
						live[who] = r
					} else {
						for _, c := range live[who].cleanupArgs() {
							f.run(t, c)
						}
						delete(live, who)
					}
					for who, r := range live {
						if !ok {
							// No ctrl spelling: nothing is installed and the plain
							// attach is used, so the shared session is untouched.
							assert.Equal(t, []string{"attach-session", "-t", "login"}, r.attachArgs())
							continue
						}
						assert.Equal(t, []string{want}, f.detachKeys(r.group),
							"%s: client %s after %s", spec, who, step)
					}
					if ok {
						assert.Empty(t, f.detachKeys("login"), "the shared session never carries a detach binding")
						assert.Equal(t, "C-b", f.sessions["login"].opts["prefix"], "the shared session's prefix is untouched")
					}
				}
				if ok {
					assert.Empty(t, f.tables, "no binding remains once every client has left")
					assert.Len(t, f.sessions, 1, "only the shared session remains")
				}
			})
		}
	}
}

// ctrl-b: the prefix would win over the detach binding, so the takeover disables
// its prefix rather than let the configured key silently lose; every other key
// keeps tmux's own escape.
func TestTakeoverPrefixNeverShadowsTheDetachKey(t *testing.T) {
	cb, _ := config.ParseDetachKey("ctrl-b")
	r := newTakeoverRoute("login", cb, "ctrl+b")
	f := newFakeTmux("login")
	f.run(t, r.attachArgs())
	assert.Equal(t, "None", f.sessions[r.group].opts["prefix"])
	assert.Equal(t, "None", f.sessions[r.group].opts["prefix2"])
	assert.Equal(t, []string{"C-b"}, f.detachKeys(r.group))

	cw, _ := config.ParseDetachKey("ctrl-w")
	r2 := newTakeoverRoute("login", cw, "ctrl+w")
	f.run(t, r2.attachArgs())
	assert.Equal(t, "C-b", f.sessions[r2.group].opts["prefix"], "tmux's own escape keeps working")
	assert.Equal(t, "None", f.sessions[r2.group].opts["prefix2"], "a user's second prefix can never shadow the detach key")
	assert.Equal(t, []string{"C-w"}, f.detachKeys(r2.group))
}

// Both takeovers build through the one routing owner, and the cleanup retires the
// client's own table and private session only.
func TestBareAttachCleanupTouchesOnlyItsOwnClient(t *testing.T) {
	cmd := execConfigAgentAttach("af-config-9", "/sock")
	var ran [][]string
	prev := runBareAttachCleanup
	runBareAttachCleanup = func(c *exec.Cmd) error { ran = append(ran, c.Args); return nil }
	t.Cleanup(func() { runBareAttachCleanup = prev })
	bareAttachCleanup(cmd)
	bareAttachCleanup(cmd) // already retired: a no-op

	require.Len(t, ran, 2)
	group := cmd.Args[slices.Index(cmd.Args, "-s")+1]
	assert.Equal(t, []string{"tmux", "-S", "/sock", "unbind-key", "-a", "-T", "af-detach-" + group}, ran[0])
	assert.Equal(t, []string{"tmux", "-S", "/sock", "kill-session", "-t", group}, ran[1])
	assert.True(t, strings.HasPrefix(group, "af-takeover-"))
	assert.NotEqual(t, group, execAccountLoginAttach("af-config-9", "/sock").Args[slices.Index(execAccountLoginAttach("af-config-9", "/sock").Args, "-s")+1],
		"every client gets its own private session")
}
