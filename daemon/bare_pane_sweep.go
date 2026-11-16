package daemon

import (
	"regexp"
	"slices"
	"strings"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/accountlogin"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// barePaneNamePatterns selects only the exact session names the two bare-pane
// classes can derive — af_ + the name each spawner hands NewTmuxSession. The
// match is a CANDIDATE test, never membership: a listed session still has to
// prove its heritage through tmux.ReapProvenBareSession before it is touched.
// Keeping the shapes exact means an unrelated af_ session — a user's own, an
// instance pane — is never even asked for its markers.
//
//   - config agents: NewTmuxSession("af-config-<n>") → af_af-config-<n>, <n>
//     the per-daemon counter (daemon/configagent.go).
//   - account logins: NewTmuxSession(agentaccount.LoginSessionName) →
//     af_af-loginx-<agent>-<hexname> (internal/agentaccount/login.go).
//
// The legacy af_af-login-<agent>-<rawname> shape is deliberately NOT selected:
// a raw account name accepts the same characters ValidateName permits, so the
// shape cannot be told apart from an instance tab's <name>__<tab> suffix —
// and a missing AF_INSTANCE_ID does not distinguish a bare pane from a tab
// (#7433 review). Any genuinely orphaned legacy pane is a two-month-old
// leftover af doctor reports; it is never reaped here.
var barePaneNamePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^` + regexp.QuoteMeta(tmux.TmuxPrefix+configAgentSessionPrefix) + `[0-9]+$`),
	regexp.MustCompile(`^` + regexp.QuoteMeta(tmux.TmuxPrefix+"af-loginx-") + `.+-[0-9a-f]+$`),
}

// barePaneCandidateName reports whether name has one of the exact shapes af
// derives for its daemon-owned bare panes. A name containing "__" is never a
// candidate: "__" is the instance-tab separator (session/tab.go), so such a
// name can only ever be an instance TAB's session — and a tab carries
// AF_SESSION, AF_HOME and AF_SESSION_GEN with no AF_INSTANCE_ID, which is
// exactly the bare-pane marker set. The name test is where the two are told
// apart (#7433 review).
func barePaneCandidateName(name string) bool {
	if strings.Contains(name, "__") {
		return false
	}
	return slices.ContainsFunc(barePaneNamePatterns, func(re *regexp.Regexp) bool {
		return re.MatchString(name)
	})
}

// Seams for the sweep's own tests: the listing, the guarded reap, and the
// pending-create journal read are package vars so a test can drive every
// membership decision without a tmux server.
var (
	barePaneListSessions   = tmux.ListSessionNames
	barePaneReap           = tmux.ReapProvenBareSession
	barePanePendingMarkers = readPendingCreateMarkers
	// The account-login supervisor lives in another package, so its ownership
	// answer is a seam like the others: a test can prove the claim check
	// consults it without needing a live login pane.
	barePaneAccountLoginOwns = (*accountlogin.Supervisor).OwnsTmuxName
)

// sweepOrphanBarePanes reaps the daemon-owned bare tmux sessions a previous
// daemon generation abandoned: config-agent panes (af_af-config-<n>) and
// account-login panes (af_af-loginx-… / af_af-login-…), which carry no instance
// row and no durable supervisor state (#7428). A SIGKILL, a panic, or a
// teardown that outlived its stop deadline leaves them live on the shared tmux
// server — the in-memory supervisor maps that own them die with the process.
//
// It runs in the startup block AFTER the restore (so restored instance rows
// can claim the names their sessions carry) and BEFORE finishInstanceRestore
// opens readiness: every path that can create a bare pane is gated on
// m.Ready() — SpawnConfigAgent and the account-login RPCs sit behind
// requireStateMutationAdmission — so a session listed here can only belong to
// a previous generation of this daemon, never to one this daemon just started.
//
// Membership is proven per session, never by name or ancestry: a candidate
// must carry this home's AF_HOME, its own AF_SESSION, and a generation marker
// on its exact incarnation — and must NOT carry an instance id, which would
// make it an instance create the pending-creates sweep owns. Anything doubtful
// — a missing marker, an unreadable environment, a foreign home, a replaced
// incarnation, an unanswerable listing — is left alone and logged for
// af doctor.
//
// The sweep runs NOTHING while any repo's instances.json failed to load:
// instance tabs carry the same bare-pane marker set (AF_SESSION/AF_HOME/
// AF_SESSION_GEN, no AF_INSTANCE_ID), and only a restored row claims their
// names. A skipped repo makes that claim set incomplete in exactly the way a
// name match cannot cover, so the whole sweep refuses — the same guard
// sweepStartupOrphanSandboxDirs runs (#7433 review).
func (m *Manager) sweepOrphanBarePanes() {
	m.mu.Lock()
	skipped := len(m.skippedRepos)
	m.mu.Unlock()
	if skipped > 0 {
		log.WarningLog.Printf("bare pane sweep: skipped; %d repo(s) did not load, so the set of owned tmux names is incomplete", skipped)
		return
	}
	home, err := config.GetConfigDir()
	if err != nil {
		log.WarningLog.Printf("bare pane sweep: cannot resolve the AF home: %v", err)
		return
	}
	exec := cmd.MakeExecutor()
	names, err := barePaneListSessions(exec)
	if err != nil {
		log.WarningLog.Printf("bare pane sweep: cannot list tmux sessions; leaving every bare-shaped session untouched: %v", err)
		return
	}
	markers, markersErr := barePanePendingMarkers()
	if markersErr != nil {
		// The journal may name sessions the unrecorded-create sweep already owns.
		// Continuing is still safe: a minted create's session carries its
		// AF_INSTANCE_ID, which the marker check below refuses on its own — the
		// claim lookup is the belt for leftovers too old to carry one.
		log.WarningLog.Printf("bare pane sweep: cannot read the pending-create journal: %v", markersErr)
		markers = nil
	}
	instances := m.instanceList()
	rows := make([]session.InstanceData, 0, len(instances))
	for _, inst := range instances {
		rows = append(rows, inst.ToInstanceData())
	}
	for _, name := range names {
		if !barePaneCandidateName(name) {
			continue
		}
		if reason := m.barePaneClaimReason(name, rows, markers); reason != "" {
			log.InfoLog.Printf("bare pane sweep: leaving %q: %s", name, reason)
			continue
		}
		reap := barePaneReap(exec, "", tmux.BareSessionProof{SessionName: name, Home: home})
		if !reap.Stopped {
			log.WarningLog.Printf("bare pane sweep: %q was not reaped (%s); leaving it for af doctor", name, reap.Why)
			continue
		}
		log.InfoLog.Printf("bare pane sweep: reaped orphaned daemon pane %q", name)
	}
}

// barePaneClaimReason names the live claim that keeps a bare-shaped session
// untouched, or "" when nothing this daemon knows about owns the name: a pane
// one of this daemon's own supervisors tracks, a tmux name a restored instance
// row claims (its own TmuxName, a tab's, or the unscoped title derivation a
// row without tmux fields carries), or a session the pending-create journal
// still holds — the unrecorded-create sweep's case, not this one's. rows is
// the instance snapshot the sweep took once.
func (m *Manager) barePaneClaimReason(name string, rows []session.InstanceData, markers []pendingCreateMarker) string {
	if m.configAgents.ownsTmuxName(name) {
		return "tracked by this daemon's config-agent supervisor"
	}
	if barePaneAccountLoginOwns(m.accountLogins, name) {
		return "tracked by this daemon's account-login supervisor"
	}
	for i := range rows {
		if taskFireRowOwnsTmuxName(&rows[i], name) {
			return "claimed by a restored instance row"
		}
	}
	for _, marker := range markers {
		if marker.SessionName == name {
			return "claimed by an unsettled pending-create marker"
		}
	}
	return ""
}
