package session

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #6254: within a restore pass, no tab's liveness may be decided with pre-spawn
// semantics while its socket is unverified. A non-account row has a live agent,
// a sibling shell refused with tmux.ErrSessionSocketUnverified, a second shell
// that answers, and a process tab whose socket is gone under a possibly-live
// server. The process tab read as absent (ProbeSession: socket-ENOENT is plain
// absence) and was marked inert, in an order-dependent way: a process tab
// restored before the refused shell was already inert when the refusal returned.
// Whatever the tab order, nothing may be marked inert, nothing may be spawned,
// and the row loads Lost.
func TestFromInstanceData_UnverifiedSocketNeverMarksATabInert(t *testing.T) {
	const agentName = "af_probe_semantics_agent"
	name := func(n string) string { return agentName + tmuxTabSeparator + n }
	shell := TabData{Name: "refused", Kind: TabKindShell, TmuxName: name("refused")}
	live := TabData{Name: "live", Kind: TabKindShell, TmuxName: name("live")}
	process := TabData{Name: "build", Kind: TabKindProcess, Command: "sleep 600", TmuxName: name("build")}

	orders := map[string][]TabData{
		"process tab before the refused shell": {process, shell, live},
		"process tab after the refused shell":  {shell, live, process},
		"process tab between the shells":       {shell, process, live},
	}
	for label, siblings := range orders {
		t.Run(label, func(t *testing.T) {
			log.Initialize(false)
			defer log.Close()
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			t.Cleanup(tmux.PinServerProbeForTest(os.Getpid()))

			var newSessions int
			exec := socketGoneExceptExec(t, map[string]bool{agentName: true, name("live"): true}, &newSessions)
			pty := persistPtyFactory{t: t, cmdExec: exec}
			prev := restoreTmuxSession
			restoreTmuxSession = func(n, program string) *tmux.TmuxSession {
				return tmux.NewTmuxSessionFromSanitizedNameWithDeps(n, program, pty, exec)
			}
			defer func() { restoreTmuxSession = prev }()

			data := deadInstanceData(t, Running, agentName, name("refused"))
			data.Tabs = append([]TabData{{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName}}, siblings...)

			restored, err := FromInstanceData(data)
			require.NoError(t, err, "a refused restore keeps the row")
			require.NotNil(t, restored)

			assert.Equal(t, Lost, restored.GetStatus(), "the refusal routes the row Lost")
			assert.Equal(t, 0, newSessions, "nothing is spawned beside a possibly-live server")
			for _, tab := range restored.GetTabs() {
				assert.False(t, tab.inert,
					"tab %q was marked inert: a possibly-live tab behind an unverified socket must stay unknown", tab.Name)
			}
		})
	}
}
