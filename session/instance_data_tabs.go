package session

import "github.com/sachiniyer/agent-factory/session/tmux"

// tabDataLocked projects one roster tab into its serializable TabData — the row
// shape InstanceData.Tabs carries and the agent-server's /v1/agent/tabs roster
// shares (#5571 W4.3), so the snapshot projection and the wire roster cannot
// drift. Caller holds i.mu.
func (i *Instance) tabDataLocked(tab *Tab) TabData {
	td := TabData{ID: tab.ID, Gen: tab.gen, Name: tab.Name, Kind: tab.Kind, Command: tab.Command, URL: tab.URL, Proposal: tab.Proposal, PushMode: tab.PushMode}
	if tab.tmux != nil {
		td.TmuxName = tab.tmux.SanitizedName()
	}
	td.SandboxTmux = tab.sandboxTmux
	td.RestoreState = tab.restoreState
	td.Conversation = conversationDataPtr(tab.Conversation)
	if len(tab.Handoffs) > 0 {
		td.Handoffs = append([]AgentHandoff(nil), tab.Handoffs...)
	}
	td.Exit = tab.Exit.data()
	td.AccountScope = tab.accountScope
	return td
}

// TabsData projects the instance's tab roster as TabData rows — the same shape
// ToInstanceData serializes, agent tab at index 0 — so a peer can consume the
// roster without the rest of the instance record. It is the /v1/agent/tabs
// payload the in-sandbox agent-server reports to the driving daemon (#5571
// W4.3). It does NOT refresh process-exit evidence: stampProcessTabExit runs on
// restore/attach paths, and the roster endpoint runs RecordProcessTabExits
// first — the separation keeps a pure read a pure read.
func (i *Instance) TabsData() []TabData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]TabData, 0, len(i.Tabs))
	for _, tab := range i.Tabs {
		out = append(out, i.tabDataLocked(tab))
	}
	return out
}

// restoreLocalTabs rebuilds a local instance's tab list from persisted data.
//
//   - New format (data.Tabs present): each tab is reconstructed in order, and
//     any tab with a persisted tmux name is bound to that exact session so
//     LocalBackend.Start can reconnect it across a restart.
//   - Legacy format (no data.Tabs, written before #930 PR 2): synthesize the
//     single Agent tab from the legacy TmuxName/Program — keeping the EXACT
//     legacy tmux name so an existing live agent session survives the upgrade.
//     No shell tab is synthesized: terminal tabs are on-demand only (#1100).
func restoreLocalTabs(instance *Instance, data InstanceData) {
	if len(data.Tabs) > 0 {
		for idx, td := range data.Tabs {
			kind := tabKindForData(td.Kind)
			var ts *tmux.TmuxSession
			if td.TmuxName != "" {
				ts = restoreTmuxSession(td.TmuxName, tabProgram(kind, td.Command, data.Program))
				// Keep the restored handle consistent with a fresh process
				// tab's: the flag only matters if Start is ever invoked, and
				// after #4479 nothing invokes it — but the invariant costs
				// nothing and keeps a future Start caller honest.
				if ts != nil && kind == TabKindProcess {
					ts.SetRemainOnExit()
				}
			}
			var conversation AgentConversationData
			if td.Conversation != nil {
				conversation = *td.Conversation
			} else if idx == 0 && data.AgentConversation != nil {
				conversation = *data.AgentConversation
			}
			// Backfill a stable id for a legacy tab persisted before #1738 (no id):
			// mint one now so the restored tab is addressable by a stable id from
			// this load forward (rollforward, mirroring the InstanceData.ID backfill).
			id := td.ID
			if id == "" {
				id = newTabID()
			}
			var handoffs []AgentHandoff
			if len(td.Handoffs) > 0 {
				handoffs = append([]AgentHandoff(nil), td.Handoffs...)
			}
			instance.Tabs = append(instance.Tabs, &Tab{
				ID:                            id,
				Name:                          td.Name,
				Kind:                          kind,
				Command:                       td.Command,
				URL:                           td.URL,
				Proposal:                      td.Proposal,
				PushMode:                      td.PushMode,
				Conversation:                  conversation,
				Handoffs:                      handoffs,
				Exit:                          tabExitFromData(td.Exit),
				gen:                           td.Gen,
				tmux:                          ts,
				accountScope:                  td.AccountScope,
				accountScopeProvenanceUnknown: idx > 0 && kind.HasTmux() && ts != nil && siblingScopeUnknown(data.Account, td.AccountScope),
			})
		}
		return
	}

	// Legacy single-session format: the agent tab keeps its exact legacy name.
	// Reconstruction is not new activity. Build the unpublished instance directly,
	// just as the current-format loop above does, without invoking mutators.
	var ts *tmux.TmuxSession
	if data.TmuxName != "" {
		ts = restoreTmuxSession(data.TmuxName, data.Program)
	} else {
		ts = tmux.NewTmuxSession(data.Title, data.Program)
	}
	instance.Tabs = []*Tab{newAgentTab(ts)}
	if data.AgentConversation != nil {
		instance.Tabs[0].Conversation = *data.AgentConversation
	}
}

// metadataTabsFrom selects the persisted rows whose kind needs nothing from the
// workspace, for a backend whose workspace did not survive the restart.
//
// The agent row is excluded: the backend seeds its own on Launch, bound to the
// live runtime, and a copied record would be a second one with no runtime behind
// it. Every other kind is dropped as before — they describe a worktree and a PTY
// that no longer exist, so a row for them would be an entry every later operation
// fails on.
//
// A VS Code tab is the exception when the runtime's host runs the editor itself
// (caps.RemoteEditor, W5.4): it names no worktree path and binds no PTY, so it is
// metadata off-box and must survive the restart like the other off-box tabs (#6090).
func metadataTabsFrom(data InstanceData, caps Capabilities) []TabData {
	var out []TabData
	for _, td := range data.Tabs {
		kind := tabKindForData(td.Kind)
		if kind == TabKindAgent {
			continue
		}
		// A sandbox's shell/process row with a recorded session is kept so a
		// reattach can hand it back to a restarted agent-server (#6162).
		restorable := (kind == TabKindShell || kind == TabKindProcess) && td.SandboxTmux != "" && td.ID != "" && caps.Workspace == WorkspaceRemote
		if TabKindRequires(kind) != TabNeedsMetadataOnly && !(kind == TabKindVSCode && caps.RemoteEditor) && !restorable {
			continue
		}
		out = append(out, td)
	}
	return out
}
