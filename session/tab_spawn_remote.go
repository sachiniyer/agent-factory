package session

import (
	"errors"
	"fmt"
	"slices"
	"strings"
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
		if refused, existing, found := i.tabIDCollisionLocked(id, kind, command); found {
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
		return nil, fmt.Errorf("failed to create %s tab in the sandbox: %w", TabKindName(kind), err)
	}
	name := displayName
	if strings.TrimSpace(td.Name) != "" {
		name = td.Name
	}
	tab := &Tab{ID: id, Name: name, Kind: kind, Command: command, Exit: tabExitFromData(td.Exit)}

	i.mu.Lock()
	stale := !i.started || i.tabSpawnBlockedLocked() != nil
	colErr, existing, collide := i.tabIDCollisionLocked(id, kind, command)
	adopt := !stale && !collide
	if adopt {
		i.Tabs = append(i.Tabs, tab)
		i.touchLocked()
	}
	i.mu.Unlock()
	switch {
	case stale:
		// The session was killed or archived while the sandbox spawned the tab;
		// close it there so it does not outlive the roster that would own it.
		if cerr := rs.rc.closeTab(id); cerr != nil && !errors.Is(cerr, ErrTabGone) {
			return nil, fmt.Errorf("session was killed during tab creation (sandbox tab %q left behind: %v)", id, cerr)
		}
		return nil, fmt.Errorf("session was killed during tab creation")
	case collide:
		return existing, colErr
	}
	return tab, nil
}

// closeOffBoxTab ends the sandbox-side process of a tmux-less shell/process tab
// by stable id. ErrTabGone is success: the tab is already closed there.
func (i *Instance) closeOffBoxTab(tab *Tab) error {
	if tab.tmux != nil || (tab.Kind != TabKindShell && tab.Kind != TabKindProcess) {
		return nil
	}
	i.mu.RLock()
	rs := i.remoteTabServerCachedLocked()
	i.mu.RUnlock()
	if rs == nil || !rs.hasFeatureCached(tabAPIFeature) {
		return nil
	}
	if err := rs.rc.closeTab(tab.ID); err != nil && !errors.Is(err, ErrTabGone) {
		return fmt.Errorf("failed to close tab %q: %w", tab.Name, err)
	}
	return nil
}
