package session

// tabDataLocked projects one roster tab into its serializable TabData — the row
// shape InstanceData.Tabs carries and the agent-server's /v1/agent/tabs roster
// shares (#5571 W4.3), so the snapshot projection and the wire roster cannot
// drift. Caller holds i.mu.
func (i *Instance) tabDataLocked(tab *Tab) TabData {
	td := TabData{ID: tab.ID, Gen: tab.gen, Name: tab.Name, Kind: tab.Kind, Command: tab.Command, URL: tab.URL, Proposal: tab.Proposal, PushMode: tab.PushMode}
	if tab.tmux != nil {
		td.TmuxName = tab.tmux.SanitizedName()
	}
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
