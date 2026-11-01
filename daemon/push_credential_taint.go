package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// instanceRuntimeMachine is a package var so a test can place a restored row on a
// machine without provisioning one.
var instanceRuntimeMachine = func(i *session.Instance) (session.RuntimeMachine, bool) { return i.RuntimeMachine() }

// The machine taint (#5571 W7.8, design steer and reviews on #6433). Once code
// the operator did not place has been placed on a machine, that machine gives no
// push credential until the operator attests its users were recreated or wiped
// (`af hosts reset-trust`). The WHOLE machine, whichever login the code ran as:
// af can't prove a user is unprivileged, and a sudo-capable one reads the rest.
// It replaces tracking session ROWS through create, failure, replacement, archive
// and kill: residue of code that ran on a host outlives every one of those.
//
//   - Recorded BEFORE any of the code can run: session.PlacementRecorder, called
//     by NewInstance and by a re-provision ahead of the backend; a record that
//     can't be written refuses the provision. Recorded again with the address the
//     runtime dialed. A restored row the operator didn't place is recorded too,
//     and no push credential is answered until every one of those records is.
//   - Never UNPROVEN: while any host is licensed, a placement onto a machine af
//     can't identify is refused at placement; with none licensed, what af does
//     know of it (its names) is recorded. So no entry matches every machine.
//   - MONOTONE: nothing but a reset removes a taint. Each placement is its own
//     entry, so a reset of one machine can't discard another's.
//   - SHARED SAFELY: every read and every read-modify-write runs under an
//     exclusive flock on the ledger (config.WithFileLock), with no cached copy,
//     so a second daemon on the same AF home (an upgrade overlap) neither drops
//     nor misses a taint. A ledger that can't be read taints every machine.

const (
	pushTaintFileName = "push-credential-taints.json"
	// sandboxTrustTarget is the reset and report name of the sandbox.ssh target.
	sandboxTrustTarget = "sandbox.ssh"
)

// pushTaint is one placement on a machine, kept until reset.
type pushTaint struct {
	ID        string                  `json:"id"`
	SessionID string                  `json:"session_id"`
	Title     string                  `json:"title,omitempty"`
	Why       string                  `json:"why"`
	Identity  session.MachineIdentity `json:"identity"`
	Since     time.Time               `json:"since"`
}

func (t pushTaint) String() string {
	return fmt.Sprintf("tainted since %s by session %q (%s), %s [entry %s]", t.Since.UTC().Format(time.RFC3339), t.Title, t.SessionID, t.Why, t.ID)
}

// taintID names an entry stably: its session, its machine and when it was made.
func taintID(sessionID string, id session.MachineIdentity, since time.Time) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + strings.Join(id.Names, ",") + "\x00" + strings.Join(id.Addrs, ",") + "\x00" + since.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:6])
}

// pushTaintState is the durable taint ledger.
type pushTaintState struct {
	// path overrides where the ledger persists (tests); "" means
	// <AF home>/push-credential-taints.json.
	path string

	// restored is set once every restored row's placement has been recorded:
	// until then, no push credential is answered. restoreMu serializes retries.
	restored  atomic.Bool
	restoreMu sync.Mutex
}

func (s *pushTaintState) filePath() (string, error) {
	if s.path != "" {
		return s.path, nil
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pushTaintFileName), nil
}

// locked runs fn on the ledger read fresh under its exclusive flock; a non-nil
// write result is persisted before the lock is released.
func (s *pushTaintState) locked(fn func(taints []pushTaint) (write []pushTaint, err error)) error {
	path, err := s.filePath()
	if err != nil {
		return err
	}
	return config.WithFileLock(path, func() error {
		var taints []pushTaint
		data, err := os.ReadFile(path)
		switch {
		case os.IsNotExist(err):
			err = nil
		case err == nil:
			err = json.Unmarshal(data, &taints)
		}
		if err != nil {
			err = fmt.Errorf("reading %s: %w", path, err)
			log.ErrorLog.Printf("push credentials are withheld from every host until %s can be read (it records which machines code the operator didn't place has run on): %v", path, err)
			return err
		}
		write, err := fn(taints)
		if err != nil || write == nil {
			return err
		}
		out, err := json.Marshal(write)
		if err != nil {
			return err
		}
		return config.AtomicWriteFileRefusingLink(path, out, 0o600)
	})
}

// record adds the placement's identity as its own entry, durably, before
// returning; an identical entry for the same session is not repeated. An
// unproven identity is never recorded as such: its match-everything flag is
// dropped, and what remains (names, addresses) is recorded if anything does.
func (s *pushTaintState) record(p session.Placement, id session.MachineIdentity, now time.Time) error {
	id.Unproven = ""
	if id.Empty() {
		return nil
	}
	return s.locked(func(taints []pushTaint) ([]pushTaint, error) {
		for _, t := range taints {
			if t.SessionID == p.SessionID && sameIdentity(t.Identity, id) {
				return nil, nil
			}
		}
		return append(taints, pushTaint{ID: taintID(p.SessionID, id, now), SessionID: p.SessionID, Title: p.Title, Why: p.Why, Identity: id, Since: now}), nil
	})
}

func sameIdentity(a, b session.MachineIdentity) bool {
	return strings.Join(a.Names, ",") == strings.Join(b.Names, ",") && strings.Join(a.Addrs, ",") == strings.Join(b.Addrs, ",")
}

// matching is every taint that may be on machine id (MachineIdentity.Shares).
func (s *pushTaintState) matching(id session.MachineIdentity) ([]pushTaint, error) {
	var out []pushTaint
	err := s.locked(func(taints []pushTaint) ([]pushTaint, error) {
		for _, t := range taints {
			if t.Identity.Shares(id) {
				out = append(out, t)
			}
		}
		return nil, nil
	})
	return out, err
}

// onlyOn reports whether entry t is known to be on machine target alone: every
// address it recorded is one of target's, or, when it recorded none, a name
// matches. A reset of one machine clears nothing it can't place there.
func onlyOn(t pushTaint, target session.MachineIdentity) bool {
	if len(t.Identity.Addrs) > 0 {
		have := map[string]bool{}
		for _, a := range target.Addrs {
			have[a] = true
		}
		for _, a := range t.Identity.Addrs {
			if !have[a] {
				return false
			}
		}
		return true
	}
	names := session.MachineIdentity{Names: t.Identity.Names}
	return names.Shares(session.MachineIdentity{Names: target.Names})
}

// resetScope says which entries a reset clears: those only on one machine, one
// entry by id, or every entry.
type resetScope struct {
	machine *session.MachineIdentity
	entry   string
	all     bool
}

func (s *pushTaintState) reset(scope resetScope) ([]pushTaint, error) {
	var cleared []pushTaint
	err := s.locked(func(taints []pushTaint) ([]pushTaint, error) {
		var kept []pushTaint
		for _, t := range taints {
			switch {
			case scope.all, scope.entry != "" && t.ID == scope.entry, scope.machine != nil && onlyOn(t, *scope.machine):
				cleared = append(cleared, t)
			default:
				kept = append(kept, t)
			}
		}
		if len(cleared) == 0 {
			return nil, nil
		}
		if kept == nil {
			kept = []pushTaint{}
		}
		return kept, nil
	})
	return cleared, err
}

// taintsFor is the taints on machine id, failing closed: a ledger that can't be
// read taints every machine.
func (s *pushCredentialState) taintsFor(id session.MachineIdentity) []pushTaint {
	taints, err := s.taints.matching(id)
	if err != nil {
		return []pushTaint{{ID: "-", SessionID: "-", Title: "(unreadable ledger)", Why: "the ledger of tainted machines can't be read: " + err.Error(), Since: time.Now()}}
	}
	sort.Slice(taints, func(i, j int) bool { return taints[i].Since.Before(taints[j].Since) })
	return taints
}

// RecordPlacement is session.PlacementRecorder for the daemon's sessions. While
// any host is licensed, a placement onto a machine af can't identify is refused
// here, at placement, rather than recorded as a taint on every machine.
func (c *sandboxCredentials) RecordPlacement(p session.Placement) error {
	if c == nil || c.manager == nil {
		return fmt.Errorf("no daemon to record the placement")
	}
	if p.SessionID == "" {
		p.SessionID = c.sessionID
	}
	id := p.Machine.Identity()
	if id.Unproven != "" {
		if len(pushLicensedMachines(c.manager.Config())) > 0 {
			return fmt.Errorf("af can't establish which machine it would run on, because %s, and a host is opted into push credentials, so it won't place code you didn't place there. Fix that and try again", id.Unproven)
		}
		log.WarningLog.Printf("session %q: af can't establish which machine it runs on (%s); recording what it knows of it", p.Title, id.Unproven)
	}
	return c.manager.pushCreds.taints.record(p, id, time.Now())
}

// recordRestoredPlacements taints the machine of every restored row the operator
// didn't place. Push credentials are answered only once every one of those
// records has been written; a failure leaves them withheld, and the next request
// tries again (pushTaintsRestored).
func (m *Manager) recordRestoredPlacements() {
	m.pushCreds.taints.restoreMu.Lock()
	defer m.pushCreds.taints.restoreMu.Unlock()
	if m.pushCreds.taints.restored.Load() {
		return
	}
	ok := true
	for _, inst := range m.trackedInstances() {
		machine, has := instanceRuntimeMachine(inst)
		if !has {
			continue
		}
		why := inst.PlacementWhy(machine)
		if why == "" {
			continue
		}
		p := session.Placement{SessionID: inst.ID, Title: inst.Title, Why: why, Machine: machine}
		if err := m.pushCreds.taints.record(p, machine.Identity(), time.Now()); err != nil {
			ok = false
			log.WarningLog.Printf("session %q: could not record the machine it runs on as tainted, so push credentials stay withheld: %v", inst.Title, err)
		}
	}
	if ok {
		m.pushCreds.taints.restored.Store(true)
	}
}

// pushTaintsRestored reports whether the restore pass has recorded every restored
// placement, retrying it once when it has not.
func (m *Manager) pushTaintsRestored() bool {
	if m.pushCreds.taints.restored.Load() {
		return true
	}
	m.recordRestoredPlacements()
	return m.pushCreds.taints.restored.Load()
}

// refuseLicensingTainted refuses a config write that would newly opt a tainted
// machine into push credentials, before anything is written: a listed host
// added or changed with push_credentials on, sandbox.push_credentials turned on,
// or sandbox.ssh re-pointed while it is on. Turning a licence off, and every
// other write, is never refused.
func (m *Manager) refuseLicensingTainted(req SetConfigValueRequest) error {
	cur := m.Config()
	if cur == nil {
		cur = config.DefaultConfig()
	}
	type target struct {
		name    string
		machine session.RuntimeMachine
	}
	var targets []target
	newlyLicensed := func(name string, e config.HostConfig) {
		if !e.PushCredentials {
			return
		}
		if c, ok := cur.Hosts[name]; ok && c.PushCredentials && c.Host == e.Host && c.User == e.User && c.Port == e.Port {
			return
		}
		targets = append(targets, target{name, session.HostMachine(e)})
	}
	if e := req.HostsEdit; e != nil && e.Op == config.HostsEditAdd && e.Host != nil {
		newlyLicensed(e.Name, *e.Host)
	}
	switch config.CanonicalConfigKey(strings.TrimSpace(req.Key)) {
	case "hosts":
		var hosts map[string]config.HostConfig
		if json.Unmarshal([]byte(req.Value), &hosts) == nil {
			for name, e := range hosts {
				newlyLicensed(name, e)
			}
		}
	case "sandbox.push_credentials":
		if on, _ := strconv.ParseBool(strings.TrimSpace(req.Value)); on && !cur.SandboxPushCredentials {
			targets = append(targets, target{sandboxTrustTarget, session.RuntimeMachine{SandboxSSH: cur.SandboxSSH}})
		}
	case "sandbox.ssh":
		if cur.SandboxPushCredentials && strings.TrimSpace(req.Value) != strings.TrimSpace(cur.SandboxSSH) {
			targets = append(targets, target{sandboxTrustTarget, session.RuntimeMachine{SandboxSSH: req.Value}})
		}
	}
	for _, t := range targets {
		if taints := m.pushCreds.taintsFor(t.machine.Identity()); len(taints) > 0 {
			return fmt.Errorf("refusing to opt %s into push credentials: %s. Recreate or wipe that host's users, then run `af hosts reset-trust %s` and try again", t.name, taints[0], t.name)
		}
	}
	return nil
}

// ResetPushTrustRequest names what the operator recreated or wiped: a listed
// host's machine (or "sandbox.ssh" for the free-form target), one ledger entry
// by its id, or All.
type ResetPushTrustRequest struct {
	Host  string `json:"host,omitempty"`
	Entry string `json:"entry,omitempty"`
	All   bool   `json:"all,omitempty"`
}

// PushTaintReport is one cleared taint.
type PushTaintReport struct {
	ID        string                  `json:"id"`
	SessionID string                  `json:"session_id"`
	Title     string                  `json:"title,omitempty"`
	Why       string                  `json:"why"`
	Since     time.Time               `json:"since"`
	Identity  session.MachineIdentity `json:"identity"`
}

// ResetPushTrustResponse is the machine identity reset (for a host) and the
// taints cleared.
type ResetPushTrustResponse struct {
	Identity session.MachineIdentity `json:"identity"`
	Cleared  []PushTaintReport       `json:"cleared"`
}

// ResetPushTrust clears the taints known to be only on one host's machine, one
// entry, or, with All, every taint. It is the operator's attestation that the
// host's users were recreated or wiped.
func (s *controlServer) ResetPushTrust(req ResetPushTrustRequest, resp *ResetPushTrustResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	scope := resetScope{all: req.All, entry: strings.TrimSpace(req.Entry)}
	name := strings.TrimSpace(req.Host)
	if n := btoi(scope.all) + btoi(scope.entry != "") + btoi(name != ""); n != 1 {
		return fmt.Errorf("name exactly one of a host, an entry, or all")
	}
	if name != "" {
		cfg := s.manager.Config()
		if cfg == nil {
			return fmt.Errorf("cannot read the host list")
		}
		var target session.RuntimeMachine
		if name == sandboxTrustTarget {
			if strings.TrimSpace(cfg.SandboxSSH) == "" {
				return fmt.Errorf("sandbox.ssh is not set")
			}
			target = session.RuntimeMachine{SandboxSSH: cfg.SandboxSSH}
		} else {
			if err := config.ValidateHostPick(cfg.Hosts, name); err != nil {
				return err
			}
			target = session.HostMachine(cfg.Hosts[name])
		}
		id := target.Identity()
		if id.Unproven != "" {
			return fmt.Errorf("af can't establish which machine %s is (%s), so it can't tell which taints are on it; clear them one at a time with --entry (`af hosts check` lists their ids)", name, id.Unproven)
		}
		scope.machine = &id
		resp.Identity = id
	}
	cleared, err := s.manager.pushCreds.taints.reset(scope)
	if err != nil {
		return err
	}
	resp.Cleared = []PushTaintReport{}
	for _, t := range cleared {
		resp.Cleared = append(resp.Cleared, PushTaintReport{ID: t.ID, SessionID: t.SessionID, Title: t.Title, Why: t.Why, Since: t.Since, Identity: t.Identity})
	}
	s.manager.info().Printf("push credentials: the operator reset trust (host %q, entry %q, all=%v), clearing %d taint(s)", name, scope.entry, scope.all, len(cleared))
	return nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ResetPushTrust asks the daemon to clear taints (`af hosts reset-trust`).
func ResetPushTrust(req ResetPushTrustRequest) (ResetPushTrustResponse, error) {
	var resp ResetPushTrustResponse
	if err := callDaemon("ResetPushTrust", req, &resp); err != nil {
		return ResetPushTrustResponse{}, err
	}
	return resp, nil
}
