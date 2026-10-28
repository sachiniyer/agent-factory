package commands

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// verbTargetClass is the W9.4 audit: EVERY leaf verb of the CLI, and what it does
// when --daemon-url / AF_DAEMON_URL / a saved target names a remote daemon. A
// verb missing from this table fails TestEveryVerbHasATargetClass, so a new verb
// has to decide — in review, in one place — between following the target,
// refusing it, or being client-local by design, instead of defaulting to
// answering about this machine.
var verbTargetClass = map[string]string{
	"af": classRouted, // the TUI itself: W9.7

	"af sessions archive":      classRouted,
	"af sessions attach":       classRouted,
	"af sessions backends":     classRouted,
	"af sessions create":       classRouted,
	"af sessions get":          classRouted,
	"af sessions handoff":      classRouted,
	"af sessions kill":         classRouted,
	"af sessions list":         classRouted,
	"af sessions preview":      classRouted,
	"af sessions prune":        classRouted,
	"af sessions restore":      classRouted,
	"af sessions retry-limit":  classRouted,
	"af sessions send-prompt":  classRouted, // --all / --create refuse by name
	"af sessions tab-create":   classRouted,
	"af sessions tab-delete":   classRouted,
	"af sessions tab-rename":   classRouted,
	"af sessions tab-reorder":  classRouted,
	"af sessions tabs create":  classRouted,
	"af sessions tabs delete":  classRouted,
	"af sessions tabs rename":  classRouted,
	"af sessions tabs reorder": classRouted,
	"af sessions watch":        classRouted,
	// whoami reads the TMUX pane it runs in, which only exists on this machine.
	"af sessions whoami": classLocal,

	"af tasks add":     classRouted,
	"af tasks get":     classRouted,
	"af tasks list":    classRouted,
	"af tasks remove":  classRouted,
	"af tasks restart": classRouted,
	"af tasks show":    classRouted,
	"af tasks trigger": classRouted,
	"af tasks update":  classRouted,

	"af projects add":    classRouted,
	"af projects color":  classRouted,
	"af projects delete": classRouted,
	"af projects list":   classRouted,
	"af projects rebind": classRouted,

	"af proposal add-doc":    classRouted,
	"af proposal attach":     classRouted,
	"af proposal cat":        classRouted,
	"af proposal changes":    classRouted,
	"af proposal checkpoint": classRouted,
	"af proposal detach":     classRouted,
	"af proposal list":       classRouted,
	"af proposal path":       classRouted,
	"af proposal push-mode":  classRouted,
	"af proposal send":       classRouted,
	"af proposal start":      classRouted,
	"af proposal write":      classRouted,

	"af quota":           classRouted,
	"af daemon status":   classRouted, // the target's health route
	"af config set":      classRouted, // global form; --project refuses in-verb
	"af config unset":    classRouted,
	"af config get":      classRefusedInVerb,
	"af config list":     classRefusedInVerb,
	"af config migrate":  classRefusedInVerb,
	"af config validate": classRefusedInVerb,
	"af accounts add":    classRefusedInVerb,
	"af accounts list":   classRefusedInVerb,
	"af accounts login":  classRefusedInVerb,
	"af hosts add":       classRefusedInVerb,
	"af hosts check":     classRefusedInVerb,
	"af hosts list":      classRefusedInVerb,
	"af hosts remove":    classRefusedInVerb,

	// No daemon API stands in for these; they refuse centrally (localOnlyVerbs).
	"af token show":       classRefusedCentral,
	"af token rotate":     classRefusedCentral,
	"af daemon restart":   classRefusedCentral,
	"af daemon install":   classRefusedCentral,
	"af daemon uninstall": classRefusedCentral,
	"af daemon adopt":     classRefusedCentral,
	"af doctor":           classRefusedCentral,
	"af reset":            classRefusedCentral,

	// Client-local by design: the binary, its terminal, its own saved target,
	// static catalogs and daemon-consumed plumbing.
	"af target set":      classLocal,
	"af target show":     classLocal,
	"af target clear":    classLocal,
	"af upgrade":         classLocal, // replaces THIS client's binary
	"af bug-report":      classLocal, // a bundle of THIS client's diagnostics
	"af version":         classLocal,
	"af keys":            classLocal,
	"af debug":           classLocal,
	"af api":             classLocal, // this build's route catalog
	"af agent-server":    classLocal,
	"af ssh-relay":       classLocal,
	"af gen-docs":        classLocal,
	"af hook-guard-tmux": classLocal,
}

func collectLeaves(c *cobra.Command, out *[]string) {
	if !c.HasSubCommands() {
		*out = append(*out, c.CommandPath())
		return
	}
	// A group with its own RunE (af, af sessions tabs…) is also a verb.
	if c.Runnable() && c == rootCmd {
		*out = append(*out, c.CommandPath())
	}
	for _, sub := range c.Commands() {
		collectLeaves(sub, out)
	}
}

func TestEveryVerbHasATargetClass(t *testing.T) {
	var leaves []string
	collectLeaves(rootCmd, &leaves)
	sort.Strings(leaves)
	var missing []string
	for _, v := range leaves {
		if _, ok := verbTargetClass[v]; !ok && !strings.HasPrefix(v, "af completion") && v != "af help" {
			missing = append(missing, v)
		}
	}
	require.Empty(t, missing, "classify these verbs in verbTargetClass (routed, refused, or local-by-design): a verb with no class would answer about this machine under --daemon-url")
	in := map[string]bool{}
	for _, v := range leaves {
		in[v] = true
	}
	for v := range verbTargetClass {
		require.True(t, in[v], "verbTargetClass names %q, which is not a verb of the CLI (renamed or removed?)", v)
	}
}

// The central class and the table that enforces it must agree both ways.
func TestCentralRefusalTableMatchesTheClass(t *testing.T) {
	for verb, class := range verbTargetClass {
		_, enforced := localOnlyVerbs[verb]
		require.Equal(t, class == classRefusedCentral, enforced, "%s: class %q vs localOnlyVerbs", verb, class)
	}
}

func TestCentrallyRefusedVerbsNameTheDaemonTheyDeclined(t *testing.T) {
	t.Setenv("AF_DAEMON_URL", "http://box.example:8443")
	for verb := range localOnlyVerbs {
		c, _, err := rootCmd.Find(strings.Fields(verb)[1:])
		require.NoError(t, err, verb)
		require.Equal(t, verb, c.CommandPath())
		err = refuseLocalOnlyUnderRemoteTarget(c)
		require.Error(t, err, verb)
		for _, want := range []string{verb, "local-only", "the remote daemon at http://box.example:8443", "--daemon-url/AF_DAEMON_URL", "on the daemon host"} {
			require.Contains(t, err.Error(), want, verb)
		}
	}
	// Control: with no remote target the same verbs are untouched.
	t.Setenv("AF_DAEMON_URL", "")
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	for verb := range localOnlyVerbs {
		c, _, _ := rootCmd.Find(strings.Fields(verb)[1:])
		require.NoError(t, refuseLocalOnlyUnderRemoteTarget(c), verb)
	}
}
