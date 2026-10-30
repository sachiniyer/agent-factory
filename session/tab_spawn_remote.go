package session

import (
	"errors"
	"fmt"
	"slices"
)

// Off-box shell and process tabs (#5571 W4.5). A sandbox whose agent-server
// advertises "tab-api" owns the tab's process: the daemon asks it to create the
// tab under the daemon's stable id (idempotent on retry, so a reconnect never
// re-executes a process command) and keeps a tmux-less roster row whose data
// plane is the id-addressed stream (W4.4).

const tabAPIFeature = "tab-api"

// hasFeatureCached reads the sandbox's /v1/health advertisement once it has answered.
// Definite answers only; see idNative.
func (s *remoteAgentServer) hasFeatureCached(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idPlaneKnown && slices.Contains(s.features, name)
}

// featureVerdict resolves the advertisement, asking the server once if it has
// not yet answered. A health fetch that fails leaves the verdict unknown — never
// absent — so a transient failure is not read as an old server (#6236).
func (s *remoteAgentServer) featureVerdict(name string) featureVerdict {
	s.idNative()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.idPlaneKnown:
		return featureUnknown
	case slices.Contains(s.features, name):
		return featurePresent
	}
	return featureAbsent
}

// remoteTabServerCached returns the cached remote agent-server without building
// one. Safe under i.mu (reads only); nil for a local instance or one nobody has
// asked for the server yet.
func (i *Instance) remoteTabServerCachedLocked() *remoteAgentServer {
	rs, _ := i.agentSrv.(*remoteAgentServer)
	return rs
}

// offBoxTabAPILocked reports whether this instance's sandbox is known to serve
// the tab API. It never touches the network: the advertisement is cached when
// the agent-server is provisioned, launched or snapshotted.
func (i *Instance) offBoxTabAPILocked() bool {
	rs := i.remoteTabServerCachedLocked()
	return rs != nil && rs.hasFeatureCached(tabAPIFeature)
}

func (i *Instance) offBoxTabAPI() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.offBoxTabAPILocked()
}

// addOffBoxTab creates a shell or process tab in the sandbox. It reuses the
// local create's id claim/join and display-name claim, so the retry contract
// ("same id + same shape answers the first call's outcome") is identical.
func (i *Instance) addOffBoxTab(id string, kind TabKind, command, requestedName string) (resultTab *Tab, resultErr error) {
	var claim *tabCreateClaim
	i.mu.Lock()
	for {
		if id == "" {
			// An earlier attempt at this exact request whose outcome the daemon
			// could not see keeps its id (#5958): the sandbox's id idempotency
			// then answers the one execution instead of running the command again.
			id = i.intentIDLocked(kind, command, requestedName)
		}
		if i.intentHeldByOtherShapeLocked(id, kind, command, requestedName) {
			i.mu.Unlock()
			return nil, fmt.Errorf("tab id %q is held by an earlier create of a different command whose outcome is unknown; retry that request or use another id", id)
		}
		if refused, existing, found := i.tabIDCollisionLocked(id, kind, command); found {
			// The retry resolved to a tab already on the roster (adopted by the
			// sweep): it has joined the one execution, so the intent is spent.
			i.consumeIntentLocked(id, kind, command, requestedName)
			i.mu.Unlock()
			return existing, refused
		}
		c, wait, err := i.tabCreateClaimOrJoinLocked(id, kind, command)
		if err != nil {
			i.mu.Unlock()
			return nil, err
		}
		if wait == nil {
			claim = c
			break
		}
		i.mu.Unlock()
		<-wait
		if c.tab != nil || c.err != nil {
			return c.tab, c.err
		}
		i.mu.Lock()
	}
	started := i.started
	spawnErr := i.tabSpawnBlockedLocked()
	rs := i.remoteTabServerCachedLocked()
	base := shellTabName
	if kind == TabKindProcess {
		base = processTabBaseName(requestedName, command)
	}
	displayName := uniqueTabNameReserving(i.Tabs, i.tabDisplayClaims, base)
	if i.tabDisplayClaims == nil {
		i.tabDisplayClaims = map[string]bool{}
	}
	i.tabDisplayClaims[displayName] = true
	i.mu.Unlock()
	defer func() {
		i.mu.Lock()
		delete(i.tabDisplayClaims, displayName)
		if claim != nil {
			delete(i.tabCreateInFlight, id)
			claim.tab, claim.err = resultTab, resultErr
			close(claim.done)
		}
		i.mu.Unlock()
	}()

	if spawnErr != nil {
		return nil, spawnErr
	}
	if !started {
		return nil, fmt.Errorf("cannot add a tab to a session that is not started")
	}
	if rs == nil {
		return nil, fmt.Errorf("cannot add this tab: the session's sandbox is not connected")
	}
	if id == "" {
		// The daemon is the id authority: a minted id is also the create's
		// idempotency key for any internal retry.
		id = newTabID()
	}
	td, err := rs.rc.createTab(id, TabKindName(kind), command, displayName)
	if err != nil {
		uncertain := createOutcomeUncertain(err)
		i.mu.Lock()
		if uncertain {
			i.recordIntentLocked(id, kind, command, requestedName, displayName)
		} else {
			i.consumeIntentLocked(id, kind, command, requestedName)
		}
		i.mu.Unlock()
		if uncertain {
			return nil, fmt.Errorf("failed to create %s tab in the sandbox (the sandbox may have started it; a retry reuses tab id %s and cannot run it twice): %w", TabKindName(kind), id, err)
		}
		return nil, fmt.Errorf("failed to create %s tab in the sandbox: %w", TabKindName(kind), err)
	}
	// The daemon is the name authority: the sandbox names a tab against its own
	// roster, which does not hold the daemon's web or other tmux-less tabs, so
	// adopting its name could duplicate one. The name reserved for this create is
	// kept, re-derived only if something took it since (#5962).
	name := displayName
	tab := &Tab{ID: id, Name: name, Kind: kind, Command: command, Exit: tabExitFromData(td.Exit), gen: td.Gen, sandboxTmux: td.TmuxName}

	i.mu.Lock()
	stale := !i.started || i.tabSpawnBlockedLocked() != nil
	colErr, existing, collide := i.tabIDCollisionLocked(id, kind, command)
	adopt := !stale && !collide
	i.consumeIntentLocked(id, kind, command, requestedName)
	if adopt {
		others := make(map[string]bool, len(i.tabDisplayClaims))
		for n := range i.tabDisplayClaims {
			if n != displayName {
				others[n] = true
			}
		}
		tab.Name = uniqueTabNameReserving(i.Tabs, others, displayName)
		i.Tabs = append(i.Tabs, tab)
		i.touchLocked()
	}
	i.mu.Unlock()
	if adopt {
		// The id may be a re-creation of a closed tab: lift the terminal mark so
		// streams to the new incarnation are admitted (#5963).
		rs.clearTabGone(id)
	}
	switch {
	case stale:
		// The session was killed or archived while the sandbox spawned the tab;
		// close it there so it does not outlive the roster that would own it. An
		// unconfirmed close is recorded, not just reported (#5957).
		if cerr := rs.rc.closeTab(id, td.Gen); cerr != nil && !errors.Is(cerr, ErrTabGone) {
			i.mu.Lock()
			i.pendingRemoteClose = append(i.pendingRemoteClose, RemoteTabCleanupData{TabID: id, Gen: td.Gen})
			i.touchLocked()
			i.mu.Unlock()
			return nil, fmt.Errorf("session was killed during tab creation (sandbox tab %q left behind, close pending: %v)", id, cerr)
		}
		return nil, fmt.Errorf("session was killed during tab creation")
	case collide:
		return existing, colErr
	}
	return tab, nil
}

// closeOffBoxTab ends the sandbox-side process of a tmux-less shell/process tab
// by stable id. ErrTabGone is success: the tab is already closed there. When the
// sandbox cannot be asked (not connected, or it does not answer) the error is
// returned and the caller's staged pending-close record keeps the id until a
// later sweep gets the sandbox's confirmation (#5957).
func (i *Instance) closeOffBoxTab(tab *Tab) error {
	if tab.tmux != nil || (tab.Kind != TabKindShell && tab.Kind != TabKindProcess) {
		return nil
	}
	i.mu.RLock()
	remote := i.capabilitiesLocked().Workspace == WorkspaceRemote
	rs := i.remoteTabServerCachedLocked()
	i.mu.RUnlock()
	if !remote {
		return nil
	}
	if rs == nil || !rs.hasFeatureCached(tabAPIFeature) {
		return fmt.Errorf("failed to close tab %q: the session's sandbox is not connected; the close is pending", tab.Name)
	}
	if err := rs.rc.closeTab(tab.ID, tab.gen); err != nil && !errors.Is(err, ErrTabGone) {
		return fmt.Errorf("failed to close tab %q: %w", tab.Name, err)
	}
	return nil
}

// ReconcileTabManagementSnapshot adopts the daemon's projected tab-management
// verdict onto a client row, reporting whether it changed. nil (an older daemon)
// leaves the row's own derivation in force (#5953).
func (i *Instance) ReconcileTabManagementSnapshot(v *bool) bool {
	if v == nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.projectedTabManagement == *v {
		return false
	}
	// Report a change only when the capability the TUI renders from differs: a
	// local row already manages tabs, so recording the projection is invisible.
	before := i.capabilitiesLocked().TabManagement
	i.projectedTabManagement = *v
	if i.capabilitiesLocked().TabManagement == before {
		return false
	}
	i.touchLocked()
	return true
}
