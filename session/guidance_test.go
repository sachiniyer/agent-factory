package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The full projection renders the reference af has always injected, byte for
// byte — generated guidance must not change what a fully capable session reads.
func TestGuidanceFullCapsRenderTheUnchangedReference(t *testing.T) {
	assert.Equal(t, afUsageReference, fullGuidanceCaps.Reference())
	assert.Equal(t, afUsageReferenceCallbackless, noCallbackGuidanceCaps.Reference())
	assert.Equal(t, afTabsSectionFull, strings.Join([]string{
		afTabsLead + afTabsRestrictions + afTabsTail, afTabLineShell, afTabLineProcess,
		afTabLineWeb, afTabLineVSCode, afTabLineDelete}, "\n"))
	require.Contains(t, afUsageReference, afTabsSectionFull, "the section constant must match afUsageBody")
	require.Contains(t, afUsageReference, afSelfArchiveLine, "the self-archive line constant must match afUsageBody")
}

// The pin: for every capability set a session can really have — the local
// runtime, the remote runtime the daemon holds a sandbox as, and each with or
// without a callback — a verb is in the rendered guidance exactly when the
// projection says it works.
func TestGuidanceFollowsTheCapabilityProjection(t *testing.T) {
	cases := map[string]struct {
		callbackUnavailable bool
		caps                Capabilities
	}{
		"local":                 {false, (&LocalBackend{}).Capabilities()},
		"sandbox (remote)":      {false, (&remoteAgentBackend{}).Capabilities()},
		"local, no callback":    {true, (&LocalBackend{}).Capabilities()},
		"sandbox, no callback":  {true, (&remoteAgentBackend{}).Capabilities()},
		"no archive capability": {false, Capabilities{Workspace: WorkspaceLocalWorktree, TabManagement: true}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ref := guidanceCapsFor(tc.callbackUnavailable, tc.caps).Reference()
			allowed := map[string]bool{}
			for _, a := range tabKindAllowances(tc.caps) {
				allowed[a.Kind] = a.Allowed && !tc.callbackUnavailable
			}
			for kind, line := range map[string]string{
				"shell":  "tab-create <title> --kind shell",
				"web":    "tab-create <title> --kind web",
				"vscode": "tab-create <title> --kind vscode",
			} {
				assert.Equal(t, allowed[kind], strings.Contains(ref, line), "tab kind %s", kind)
			}
			// A process tab follows the shell verdict — the projection has no kind for it.
			assert.Equal(t, allowed["shell"], strings.Contains(ref, "tab-create <title> --command"), "process tab")
			anyTab := allowed["shell"] || allowed["web"] || allowed["vscode"]
			assert.Equal(t, anyTab, strings.Contains(ref, "tab-delete"), "tab-delete only when a tab can be made")

			// A remote workspace admits only external HTTPS web tabs, so the
			// guidance must not teach the --port workflow the daemon refuses.
			if allowed["web"] {
				refusesLoopback := tc.caps.RefuseTabKind(TabKindWeb, "http://localhost:3000") != nil
				assert.Equal(t, !refusesLoopback, strings.Contains(ref, "--port"), "web --port alternative")
				assert.Equal(t, refusesLoopback, strings.Contains(ref, "only external HTTPS"), "HTTPS-only restriction stated")
			}

			selfArchive := tc.caps.Archive && !tc.callbackUnavailable
			assert.Equal(t, selfArchive, strings.Contains(ref, "archive --self"), "self-archive")
			assert.Equal(t, !tc.callbackUnavailable, strings.Contains(ref, "af sessions whoami"), "callback verbs")

			// The proposal and backlog routes admit no sandbox credential
			// (TestSandboxScopeTableIsPinned in daemon), so only an agent in a
			// local worktree, which calls af over the host's own socket, is
			// taught their verbs or given the proposal skill.
			hostStores := !tc.callbackUnavailable && tc.caps.Workspace == WorkspaceLocalWorktree
			g := guidanceCapsFor(tc.callbackUnavailable, tc.caps)
			for _, verb := range []string{"af proposal start", "af backlog list", "af backlog add --file", "af backlog done --self", "af backlog ask --self"} {
				assert.Equal(t, hostStores, strings.Contains(ref, verb), "%s", verb)
			}
			assert.Equal(t, hostStores, len(pluginSkills(g)) > 0, "the proposal skill")
			if !hostStores && !tc.callbackUnavailable {
				assert.NotContains(t, ref, "\n\n\n", "a dropped section must take its separating blank line with it")
				assert.Contains(t, ref, "manage its login autostart.\n\nRoot-agent configuration", "tasks then root agent, one blank line")
				assert.Contains(t, ref, "no code changes\".\n\nAsking for the user:", "body then attention, one blank line")
			}
		})
	}
}

// A sandbox session's guidance is rendered from the REMOTE runtime's tab
// admission even though its instance runs the local runtime inside the sandbox:
// the daemon admits its tabs, and it refuses shell/process there (a vscode tab
// opens the editor on the host, W5.4).
func TestInSandboxInstanceRendersFromTheRemoteProjection(t *testing.T) {
	in := &Instance{hostedRemotely: true}
	caps := in.guidanceCaps()
	assert.True(t, caps.Callback)
	assert.False(t, caps.Shell, "the daemon refuses shell tabs on a remote workspace")
	assert.True(t, caps.VSCode, "the host runs the editor for a remote workspace (W5.4)")
	assert.True(t, caps.Web, "an external HTTPS web tab works on a remote workspace")
	assert.False(t, caps.HostStores, "the sandbox credential reaches no proposal or backlog route")
	assert.NotContains(t, caps.Reference(), "af backlog")
	assert.NotContains(t, caps.Reference(), "af proposal")

	local := (&Instance{}).guidanceCaps()
	assert.Equal(t, fullGuidanceCaps, local)

	in.callbackUnavailable = "no credential"
	assert.Equal(t, noCallbackGuidanceCaps, in.guidanceCaps())
}

// A sandbox whose agent-server advertises tab-api serves shell and process tabs
// off-box (W4.5), so its guidance teaches them (#5878); one that does not
// advertise it keeps the refusal.
func TestHostedGuidanceFollowsTheAdvertisedTabAPI(t *testing.T) {
	in := &Instance{hostedRemotely: true, advertisedFeatures: []string{tabAPIFeature, tabIDAddressingFeature}}
	caps := in.guidanceCaps()
	assert.True(t, caps.Callback)
	assert.True(t, caps.Shell, "an advertised tab-api means shell tabs work")
	assert.Contains(t, caps.Reference(), "--kind shell")

	old := &Instance{hostedRemotely: true, advertisedFeatures: []string{tabIDAddressingFeature}}
	assert.False(t, old.guidanceCaps().Shell)
}

// Each distinct projection gets its own generated file, so two sessions with
// different capabilities on one host never overwrite the guidance the other
// reads — the flap #5577 closed for the callbackless pair.
func TestGuidanceVariantsNeverShareAFile(t *testing.T) {
	seen := map[string]guidanceCaps{}
	for _, cb := range []bool{false, true} {
		for _, sa := range []bool{false, true} {
			for _, sh := range []bool{false, true} {
				for _, w := range []bool{false, true} {
					for _, vs := range []bool{false, true} {
						for _, hs := range []bool{false, true} {
							g := guidanceCaps{Callback: cb, SelfArchive: sa, Shell: sh, Web: w, VSCode: vs, HostStores: hs}
							if !cb && g != noCallbackGuidanceCaps {
								continue // no callback renders one doc whatever else is set
							}
							k := g.key()
							if prev, dup := seen[k]; dup {
								t.Fatalf("variants %+v and %+v share key %q", prev, g, k)
							}
							seen[k] = g
						}
					}
				}
			}
		}
	}
	assert.Equal(t, "", fullGuidanceCaps.key(), "the full variant keeps its historical path")
	assert.Equal(t, "callbackless", noCallbackGuidanceCaps.key())
}
