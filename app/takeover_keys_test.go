package app

import (
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// fakeTmux interprets the takeover's tmux command sequence with the semantics the
// routing relies on, so the effective detach key of every client can be asserted
// at each step: bindings and options are per session/table, a client attached to
// session G consults G's key-table, and the prefix is checked BEFORE the table
// (server-client.c: "prefix always takes precedence").
type fakeTmux struct {
	sessions map[string]*fakeSession
	failed   bool
	tables   map[string]map[string]string // table -> key -> command
}

type fakeSession struct {
	local   map[string]string // options set on this session itself (what show-options prints)
	opts    map[string]string
	grouped string
}

func newFakeTmux(shared ...string) *fakeTmux {
	f := &fakeTmux{sessions: map[string]*fakeSession{}, tables: map[string]map[string]string{
		// What the session's normal pane runs on: tmux's defaults plus the mouse
		// wheel and drag bindings, and one on a key a detach_keys value can name.
		"root": {"WheelUpPane": "copy-mode -e", "WheelDownPane": "send-keys -M", "MouseDrag1Pane": "copy-mode -M", "MouseDown1Pane": "select-pane -t =", "C-x": "user-binding"},
	}}
	for _, s := range shared {
		f.sessions[s] = &fakeSession{
			local: map[string]string{"mouse": "on", "history-limit": "50000"},
			opts:  map[string]string{"prefix": "C-b", "key-table": "root", "mouse": "on", "history-limit": "50000"},
		}
	}
	return f
}

// run applies one `;`-separated command group.
func (f *fakeTmux) run(t *testing.T, args []string) {
	t.Helper()
	f.failed = false
	for _, c := range splitTmuxSequence(args) {
		if f.failed {
			return
		}
		switch c[0] {
		case "has-session":
			require.Equal(t, "-t", c[1])
			require.True(t, strings.HasPrefix(c[2], "="), "exact-match target: %v", c)
			if _, ok := f.sessions[strings.TrimPrefix(c[2], "=")]; !ok {
				f.failed = true // tmux aborts the rest of the sequence
				return
			}
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
			// A grouped session starts from the server's GLOBAL options: mouse off.
			f.sessions[group] = &fakeSession{grouped: of, local: map[string]string{}, opts: map[string]string{"prefix": "C-b", "key-table": "root", "mouse": "off"}}
		case "set-option":
			require.Equal(t, "-t", c[1])
			sess, ok := f.sessions[c[2]]
			require.True(t, ok, "set-option targets an existing session: %v", c)
			sess.opts[c[3]] = strings.Join(c[4:], " ")
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
		case "run-shell":
			// The mirror: retarget `list-keys -T root` into this client's table.
			script := c[1]
			if strings.Contains(script, "show-options") {
				// The session-options mirror: the source's locally set options land on
				// the client's private session.
				m := regexp.MustCompile(`set-option -t (af-takeover-\S+) `).FindStringSubmatch(script)
				require.NotNil(t, m, "the options mirror names the client's own session: %s", script)
				src := f.sessions[strings.Trim(regexp.MustCompile(`show-options -t '=([^']+):'`).FindStringSubmatch(script)[1], "'")]
				require.NotNil(t, src)
				for k, v := range src.local {
					f.sessions[m[1]].opts[k] = v
				}
				continue
			}
			require.Contains(t, script, "list-keys -T root")
			m := regexp.MustCompile(`-T (af-detach-\S+) /`).FindStringSubmatch(script)
			require.NotNil(t, m, "the mirror names the client's own table: %s", script)
			f.tables[m[1]] = map[string]string{}
			for k, v := range f.tables["root"] {
				f.tables[m[1]][k] = v
			}
		case "attach-session":
			_, ok := f.sessions[c[2]]
			require.True(t, ok, "attach targets the private session: %v", c)
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
					assert.Len(t, f.tables, 1, "only root remains once every client has left")
					assert.Contains(t, f.tables, "root")
					assert.Equal(t, "user-binding", f.tables["root"]["C-x"], "root itself is never modified")
					assert.NotEqual(t, "detach-client", f.tables["root"][want], "the detach binding never lands in root")
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

// The has-session guard: a login that vanished between the daemon's answer and
// the takeover is an error again, never a fresh unrelated shell (new-session -t
// accepts a missing target and creates one).
func TestTakeoverRefusesAVanishedLoginSession(t *testing.T) {
	cw, _ := config.ParseDetachKey("ctrl-w")
	r := newTakeoverRoute("gone", cw, "ctrl+w")
	assert.Equal(t, []string{"has-session", "-t", "=gone", ";"}, r.attachArgs()[:4],
		"the guard comes first, exact-match")

	f := newFakeTmux() // the login session no longer exists
	f.run(t, r.attachArgs())
	assert.True(t, f.failed)
	assert.Empty(t, f.sessions, "nothing was created, no stray shell")
	assert.Len(t, f.tables, 1, "only root; no table was created")
}

// Everything that routes keys or shows the hint runs before the attach; commands
// queued after an attaching command only run once that client has gone.
func TestTakeoverConfiguresBeforeItAttaches(t *testing.T) {
	cw, _ := config.ParseDetachKey("ctrl-w")
	args := newTakeoverRoute("login", cw, "ctrl+w").attachArgs()
	assert.Equal(t, "attach-session", args[len(args)-3], "the attach is the last command")
	assert.Equal(t, []string{"attach-session", "-t"}, args[len(args)-3:len(args)-1])
	assert.NotContains(t, args[len(args)-3:], ";")
	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "status-right ctrl+w leaves")
	assert.Less(t, strings.Index(joined, "bind-key"), strings.Index(joined, "attach-session"))
	// #6193: tmux's destroy-unattached reaper took the whole server down
	// intermittently; teardown is the explicit cleanup, never the option.
	assert.NotContains(t, joined, "destroy-unattached")
}

// #6221: the mirrors replay whatever the source session set locally, so no option
// the takeover depends on may be set before them. Every set-option and bind-key
// must follow the last run-shell, and the routing set is pinned by name so a new
// routing option has to be added here, after the mirrors.
func TestTakeoverRoutingOptionsAreAppliedAfterEveryMirror(t *testing.T) {
	cw, _ := config.ParseDetachKey("ctrl-w")
	args := newTakeoverRoute("login", cw, "ctrl+w").attachArgs()
	lastMirror, routing := -1, map[string]bool{}
	var cmds [][]string
	for _, c := range strings.Split(strings.Join(args, "\x00"), "\x00;\x00") {
		cmds = append(cmds, strings.Split(c, "\x00"))
	}
	for i, c := range cmds {
		if c[0] == "run-shell" {
			lastMirror = i
		}
	}
	require.GreaterOrEqual(t, lastMirror, 0)
	for i, c := range cmds {
		switch c[0] {
		case "set-option":
			routing[c[3]] = true
			assert.Greater(t, i, lastMirror, "set-option %s runs before a mirror and could be overwritten", c[3])
		case "bind-key":
			assert.Greater(t, i, lastMirror, "bind-key runs before the root mirror")
		}
	}
	assert.Equal(t, map[string]bool{"prefix": true, "prefix2": true, "key-table": true, "status-right": true}, routing)
}

// #6073: a takeover pane keeps every binding the session's normal pane has —
// mouse wheel and selection included — and adds only af's detach route; the
// configured key wins over a root binding on the same key.
func TestTakeoverKeepsEveryRootBindingAndAddsOnlyTheDetachRoute(t *testing.T) {
	cw, _ := config.ParseDetachKey("ctrl-w")
	r := newTakeoverRoute("login", cw, "ctrl+w")
	f := newFakeTmux("login")
	root := map[string]string{}
	for k, v := range f.tables["root"] {
		root[k] = v
	}
	f.run(t, r.attachArgs())

	got := f.tables[r.table]
	want := map[string]string{"C-w": "detach-client"}
	for k, v := range root {
		want[k] = v
	}
	assert.Equal(t, want, got, "root's bindings (mouse wheel, drag, selection) plus the detach route, nothing else")
	for _, k := range []string{"WheelUpPane", "WheelDownPane", "MouseDrag1Pane", "MouseDown1Pane"} {
		assert.Contains(t, got, k, "mouse binding %s survives in the takeover pane", k)
	}

	// A root binding on the detach key itself loses to the detach route in the
	// client's own table, and root keeps its binding.
	f2 := newFakeTmux("login")
	f2.tables["root"]["C-w"] = "user-binding"
	r2 := newTakeoverRoute("login", cw, "ctrl+w")
	f2.run(t, r2.attachArgs())
	assert.Equal(t, "detach-client", f2.tables[r2.table]["C-w"], "the configured key wins in the private table")
	assert.Equal(t, "user-binding", f2.tables["root"]["C-w"], "root deliberately keeps its own binding")
	assert.Contains(t, r.mirrorRootScript(), "-T root +/bind-key", "the real script retargets the root listing")
}

// #6073 (mouse): the private session inherits the shared session's own options, so
// scrolling and selection survive even when the server's global mouse is off, while
// the takeover's routing options are not clobbered by the copy.
func TestTakeoverPrivateSessionKeepsTheSourceOptions(t *testing.T) {
	cw, _ := config.ParseDetachKey("ctrl-w")
	r := newTakeoverRoute("login", cw, "ctrl+w")
	f := newFakeTmux("login")
	f.run(t, r.attachArgs())

	g := f.sessions[r.group].opts
	assert.Equal(t, "on", g["mouse"], "mouse scrolling and selection work in the takeover pane")
	assert.Equal(t, "50000", g["history-limit"], "scrollback is the source's")
	assert.Equal(t, "C-b", g["prefix"])
	assert.Equal(t, r.table, g["key-table"], "the copy does not clobber the takeover's routing")
	assert.Equal(t, "ctrl+w leaves", g["status-right"])

	script := r.mirrorSessionOptionsScript()
	assert.Contains(t, script, "show-options -t '=login:'", "a pane-target command needs the trailing colon for an exact session match (#6270)")
	assert.Less(t, strings.Index(strings.Join(r.attachArgs(), " "), "show-options"), strings.Index(strings.Join(r.attachArgs(), " "), "key-table"),
		"the copy runs before the routing options so they win")
}

// The scripts handed to tmux's run-shell are parsed by sh: every word the route
// interpolates must come back as exactly the intended argv. shellQuote is the only
// quoting in the path, so round-trip hostile session names through a real sh.
func TestTakeoverShellQuotingRoundTrips(t *testing.T) {
	for _, name := range []string{
		"af_af-login-codex-work", "plain", "with space", "it's", `dq"uote`, "$(touch /tmp/x)", "a;b|c&d", "back\\slash", "star*glob", "=eq", "-dash",
	} {
		out, err := exec.Command("sh", "-c", `printf '%s\n' `+shellQuote(name)).Output()
		require.NoError(t, err, name)
		assert.Equal(t, name+"\n", string(out), "sh must hand back exactly the session name %q as one word", name)
	}

	// The scripts themselves parse as shell and name the right targets as ONE word.
	cw, _ := config.ParseDetachKey("ctrl-w")
	r := newTakeoverRoute("it's a login", cw, "ctrl+w")
	for _, script := range []string{r.mirrorSessionOptionsScript(), r.mirrorRootScript()} {
		require.NoError(t, exec.Command("sh", "-n", "-c", script).Run(), "must parse as shell: %s", script)
	}
	assert.Contains(t, r.mirrorSessionOptionsScript(), `show-options -t '=it'\''s a login:'`)
}

// #6270: a mirror that cannot read the source is never silent. The script records
// the reason on the private session; the cleanup reads it and logs a warning.
func TestTakeoverCleanupLogsAFailedOptionMirror(t *testing.T) {
	cw, _ := config.ParseDetachKey("ctrl-w")
	r := newTakeoverRoute("login", cw, "ctrl+w")
	assert.Contains(t, r.mirrorSessionOptionsScript(), mirrorErrorOption, "the script records why it failed")
	assert.Contains(t, r.mirrorSessionOptionsScript(), "|| {", "a failed show-options is handled, not swallowed")

	var warnings logtest.Buffer
	oldOut, oldFlags := log.WarningLog.Writer(), log.WarningLog.Flags()
	log.WarningLog.SetOutput(&warnings)
	log.WarningLog.SetFlags(0)
	t.Cleanup(func() { log.WarningLog.SetOutput(oldOut); log.WarningLog.SetFlags(oldFlags) })
	oldRead, oldRun := readTakeoverMirrorError, runBareAttachCleanup
	readTakeoverMirrorError = func(string, takeoverRoute) string { return "show-options on the source session failed" }
	runBareAttachCleanup = func(*exec.Cmd) error { return nil }
	t.Cleanup(func() { readTakeoverMirrorError, runBareAttachCleanup = oldRead, oldRun })

	cmd := exec.Command("true")
	registerTakeoverRoute(cmd, "", r)
	bareAttachCleanup(cmd)
	assert.Contains(t, warnings.String(), "were not mirrored")
	assert.Contains(t, warnings.String(), "show-options on the source session failed")
}
