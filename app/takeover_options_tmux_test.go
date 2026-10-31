package app

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #6270: the takeover's option mirror must really copy the source session's local
// options. This runs against a REAL tmux server (an isolated one; skipped where tmux
// is absent), because the bug was a tmux target-resolution rule the fake cannot
// model: show-options declares -t a PANE target, and the exact-match `=` is stripped
// only from the session part of a split target, so a bare `=<session>` fails.
func TestTakeoverMirrorCopiesTheSourceOptionsOnRealTmux(t *testing.T) {
	testguard.IsolateTmux(t)
	out, _ := exec.Command("tmux", "-V").Output()
	t.Logf("tmux -V: %s", strings.TrimSpace(string(out)))

	tm := func(args ...string) (stdout, stderr string, err error) {
		c := exec.Command("tmux", args...)
		var eb strings.Builder
		c.Stderr = &eb
		o, err := c.Output()
		return strings.TrimSpace(string(o)), strings.TrimSpace(eb.String()), err
	}
	must := func(args ...string) string {
		t.Helper()
		o, e, err := tm(args...)
		require.NoError(t, err, "tmux %s: %s", strings.Join(args, " "), e)
		return o
	}

	// The server's GLOBAL mouse stays off, as a default install has it.
	must("new-session", "-d", "-s", "keeper", "sleep 600")
	must("set-option", "-g", "mouse", "off")
	const src = "af_takeover_src"
	must("new-session", "-d", "-s", src, "sleep 600")
	must("set-option", "-t", src+":", "mouse", "on")
	must("set-option", "-t", src+":", "history-limit", "12345")
	id := must("display-message", "-p", "-t", "="+src+":", "#{session_id}")
	require.True(t, strings.HasPrefix(id, "$"), id)

	// Pin which -t forms show-options accepts for a session's local options.
	forms := map[string]bool{ // form -> must resolve
		"=" + src:       false, // the #6270 bug: a bare exact-match is a pane spec
		"=" + src + ":": true,
		src + ":":       true,
		id:              true, // a session id, as #6260 relies on elsewhere
	}
	for form, want := range forms {
		o, e, err := tm("show-options", "-t", form)
		t.Logf("show-options -t %-24q exit-ok=%v stdout=%q stderr=%q", form, err == nil, o, e)
		if want {
			assert.NoError(t, err, "show-options -t %s", form)
			assert.Contains(t, o, "mouse on", "show-options -t %s", form)
			assert.Contains(t, o, "history-limit 12345", "show-options -t %s", form)
		} else {
			assert.Error(t, err, "show-options -t %s must not resolve a session", form)
		}
	}

	// Run the takeover's real sequence minus the final attach (no tty here).
	cw, _ := config.ParseDetachKey("ctrl-w")
	r := newTakeoverRoute(src, cw, "ctrl+w")
	args := r.attachArgs()
	require.Equal(t, "attach-session", args[len(args)-3])
	must(args[:len(args)-4]...)

	get := func(opt string) string {
		return must("show-options", "-v", "-t", "="+r.group+":", opt)
	}
	assert.Equal(t, "on", get("mouse"), "the private session has mouse on although the global is off")
	assert.Equal(t, "12345", get("history-limit"), "the source's history limit is copied")
	assert.Equal(t, "C-b", get("prefix"), "the routing options are applied after the mirror")
	assert.Equal(t, r.table, get("key-table"))
	assert.Equal(t, "", must("show-options", "-qv", "-t", "="+r.group+":", mirrorErrorOption), "a successful mirror records no error")

	// A mirror that cannot read its source says so, and the takeover's cleanup can read it.
	bad := newTakeoverRoute("af_takeover_gone", cw, "ctrl+w")
	must("new-session", "-d", "-s", bad.group, "sleep 600")
	must("run-shell", bad.mirrorSessionOptionsScript())
	assert.Contains(t, readTakeoverMirrorError("", bad), "show-options on the source session failed")
}
