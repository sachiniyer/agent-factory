package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// The machine taint (#5571 W7.8, design steer on #6433). Once code the operator
// did not place has been placed on a machine, as a user, that machine and user
// give no push credential until the operator attests the user was recreated or
// wiped (`af hosts reset-trust`). It replaces tracking session ROWS through
// create, failure, replacement, archive and kill: residue of code that ran as a
// shared host user outlives every one of those, so only the machine can carry it.
//
//   - Recorded BEFORE any of the code can run: session.PlacementRecorder, called
//     by NewInstance and by a re-provision ahead of the backend; a record that
//     can't be written refuses the provision. Recorded again with the address the
//     runtime dialed, whether or not the create then failed. A restored row the
//     operator didn't place is recorded too (a session from before af recorded
//     taints).
//   - MONOTONE: nothing but a reset removes a taint. It survives the row and the
//     daemon (persisted, 0600, written before the record call returns).
//   - Read FAIL CLOSED: a record that can't be read taints every machine until
//     it can.
//   - Every canonical identity (session.MachineIdentity) a placement ever named is
//     kept, merged per session.

const (
	pushTaintFileName = "push-credential-taints.json"
	// sandboxTrustTarget is the reset and report name of the sandbox.ssh target.
	sandboxTrustTarget = "sandbox.ssh"
)

// pushTaint is one session's placement on a machine, kept until reset.
type pushTaint struct {
	SessionID string                  `json:"session_id"`
	Title     string                  `json:"title,omitempty"`
	Why       string                  `json:"why"`
	Identity  session.MachineIdentity `json:"identity"`
	Since     time.Time               `json:"since"`
}

func (t pushTaint) String() string {
	return fmt.Sprintf("tainted since %s by session %q (%s), %s", t.Since.UTC().Format(time.RFC3339), t.Title, t.SessionID, t.Why)
}

// pushTaintState is the durable taint record.
type pushTaintState struct {
	// path overrides where the record persists (tests); "" means
	// <AF home>/push-credential-taints.json.
	path string

	mu     sync.Mutex
	loaded bool
	taints []pushTaint

	// restored is set once the daemon's restore has recorded every restored row
	// it holds: until then, no push credential is answered.
	restored atomic.Bool
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

// loadLocked reads the record until one read succeeds. Caller holds s.mu.
func (s *pushTaintState) loadLocked() error {
	if s.loaded {
		return nil
	}
	path, err := s.filePath()
	if err != nil {
		return err
	}
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
	s.taints, s.loaded = taints, true
	return nil
}

func (s *pushTaintState) persistLocked() error {
	path, err := s.filePath()
	if err != nil {
		return err
	}
	data, err := json.Marshal(s.taints)
	if err != nil {
		return err
	}
	return config.AtomicWriteFileRefusingLink(path, data, 0o600)
}

// record taints the machine p names, durably, before returning. An identity is
// merged into the session's existing taint of the same machine, else added.
func (s *pushTaintState) record(p session.Placement, now time.Time) error {
	id := p.Machine.Identity()
	if id.Empty() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	prev := append([]pushTaint(nil), s.taints...)
	merged := false
	for i := range s.taints {
		t := &s.taints[i]
		if t.SessionID == p.SessionID && (strictlyShares(t.Identity, id) || (t.Identity.Unproven != "" && t.Identity.Unproven == id.Unproven)) {
			t.Identity = t.Identity.Merge(id)
			merged = true
			break
		}
	}
	if !merged {
		s.taints = append(s.taints, pushTaint{SessionID: p.SessionID, Title: p.Title, Why: p.Why, Identity: id, Since: now})
	}
	if err := s.persistLocked(); err != nil {
		s.taints = prev
		return err
	}
	return nil
}

// matching is every taint that may be on machine id (MachineIdentity.Shares, so
// an unproven taint matches every machine).
func (s *pushTaintState) matching(id session.MachineIdentity) ([]pushTaint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	var out []pushTaint
	for _, t := range s.taints {
		if t.Identity.Shares(id) {
			out = append(out, t)
		}
	}
	return out, nil
}

// strictlyShares is Shares on what both identities actually name: an unproven
// identity (one af couldn't establish) is not taken to be any particular machine.
// A reset attests ONE machine's user, so it clears only what is known to be on it.
func strictlyShares(a, b session.MachineIdentity) bool {
	a.Unproven, b.Unproven = "", ""
	return a.Shares(b)
}

// reset removes the taints on machine target (strictly), or every taint.
func (s *pushTaintState) reset(target session.MachineIdentity, all bool) ([]pushTaint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	var kept, cleared []pushTaint
	for _, t := range s.taints {
		if all || strictlyShares(t.Identity, target) {
			cleared = append(cleared, t)
		} else {
			kept = append(kept, t)
		}
	}
	if len(cleared) == 0 {
		return nil, nil
	}
	prev := s.taints
	s.taints = kept
	if err := s.persistLocked(); err != nil {
		s.taints = prev
		return nil, err
	}
	return cleared, nil
}

// taintsFor is the taints on machine id, failing closed: a record that can't be
// read taints every machine.
func (s *pushCredentialState) taintsFor(id session.MachineIdentity) []pushTaint {
	taints, err := s.taints.matching(id)
	if err != nil {
		return []pushTaint{{SessionID: "-", Title: "(unreadable record)", Why: "the record of tainted machines can't be read: " + err.Error(), Since: time.Now()}}
	}
	return taints
}

// RecordPlacement is session.PlacementRecorder for the daemon's sessions.
func (c *sandboxCredentials) RecordPlacement(p session.Placement) error {
	if c == nil || c.manager == nil {
		return fmt.Errorf("no daemon to record the placement")
	}
	if p.SessionID == "" {
		p.SessionID = c.sessionID
	}
	return c.manager.pushCreds.taints.record(p, time.Now())
}

// recordRestoredPlacements taints the machine of every restored row the operator
// didn't place, then lets push credentials be answered. A row from before af
// recorded taints is tainted here; one whose runtime af no longer holds is the
// trust model's to cover (docs/backends.md).
func (m *Manager) recordRestoredPlacements() {
	defer m.pushCreds.taints.restored.Store(true)
	for _, inst := range m.trackedInstances() {
		machine, ok := instanceRuntimeMachine(inst)
		if !ok {
			continue
		}
		why := inst.PlacementWhy(machine)
		if why == "" {
			continue
		}
		if err := m.pushCreds.taints.record(session.Placement{SessionID: inst.ID, Title: inst.Title, Why: why, Machine: machine}, time.Now()); err != nil {
			log.WarningLog.Printf("session %q: could not record the machine it runs on as tainted: %v", inst.Title, err)
		}
	}
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
			return fmt.Errorf("refusing to opt %s into push credentials: %s. Recreate or wipe that host user, then run `af hosts reset-trust %s` and try again", t.name, taints[0], t.name)
		}
	}
	return nil
}

// ResetPushTrustRequest names the host whose user the operator recreated or
// wiped: a listed host, "sandbox.ssh" for the free-form target, or All.
type ResetPushTrustRequest struct {
	Host string `json:"host,omitempty"`
	All  bool   `json:"all,omitempty"`
}

// PushTaintReport is one cleared taint.
type PushTaintReport struct {
	SessionID string                  `json:"session_id"`
	Title     string                  `json:"title,omitempty"`
	Why       string                  `json:"why"`
	Since     time.Time               `json:"since"`
	Identity  session.MachineIdentity `json:"identity"`
}

// ResetPushTrustResponse is the machine identity reset and the taints cleared.
type ResetPushTrustResponse struct {
	Identity session.MachineIdentity `json:"identity"`
	Cleared  []PushTaintReport       `json:"cleared"`
}

// ResetPushTrust clears the taints on one host's machine and user (strictly:
// those known to be on it), or, with All, every taint. It is the operator's
// attestation that the host user was recreated or wiped.
func (s *controlServer) ResetPushTrust(req ResetPushTrustRequest, resp *ResetPushTrustResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	var target session.MachineIdentity
	if !req.All {
		name := strings.TrimSpace(req.Host)
		cfg := s.manager.Config()
		if cfg == nil {
			return fmt.Errorf("cannot read the host list")
		}
		if name == sandboxTrustTarget {
			if strings.TrimSpace(cfg.SandboxSSH) == "" {
				return fmt.Errorf("sandbox.ssh is not set")
			}
			target = session.RuntimeMachine{SandboxSSH: cfg.SandboxSSH}.Identity()
		} else {
			if err := config.ValidateHostPick(cfg.Hosts, name); err != nil {
				return err
			}
			target = session.HostMachine(cfg.Hosts[name]).Identity()
		}
		if target.Empty() || (len(target.Names) == 0 && len(target.Addrs) == 0) {
			return fmt.Errorf("af can't establish which machine %s is (%s), so it can't tell which taints are on it; use --all once you have reset every host user they could have reached", name, target.Unproven)
		}
	}
	cleared, err := s.manager.pushCreds.taints.reset(target, req.All)
	if err != nil {
		return err
	}
	resp.Identity, resp.Cleared = target, []PushTaintReport{}
	for _, t := range cleared {
		resp.Cleared = append(resp.Cleared, PushTaintReport{SessionID: t.SessionID, Title: t.Title, Why: t.Why, Since: t.Since, Identity: t.Identity})
	}
	s.manager.info().Printf("push credentials: the operator reset trust in %q (all=%v), clearing %d taint(s)", req.Host, req.All, len(cleared))
	return nil
}

// ResetPushTrust asks the daemon to clear taints (`af hosts reset-trust`).
func ResetPushTrust(req ResetPushTrustRequest) (ResetPushTrustResponse, error) {
	var resp ResetPushTrustResponse
	if err := callDaemon("ResetPushTrust", req, &resp); err != nil {
		return ResetPushTrustResponse{}, err
	}
	return resp, nil
}
