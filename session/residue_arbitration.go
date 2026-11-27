package session

import (
	"encoding/json"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// ResidueAttribution classifies one `.af-source-<32hex>`/`.af-copy-<32hex>`
// candidate against the durable records every session in this AF home keeps
// (#7819). Records and retained-tree reports outrank git's worktree
// registration leaf: af prunes that leaf on every archive, kill and setup,
// and git recycles leaf names, so it can never prove ownership the way a
// durable af record bound to a stable instance id can.
type ResidueAttribution int

const (
	// ResidueUnattributed means no durable record names the candidate — for
	// example residue a crash left before records existed. The caller then
	// falls back to master's leaf inference, which applies only to residue
	// no record names.
	ResidueUnattributed ResidueAttribution = iota
	// ResidueOursQuarantine means THIS session's move record names the
	// candidate as its `.af-source-*` quarantine: the complete original
	// worktree, including anything an incomplete copy skipped.
	ResidueOursQuarantine
	// ResidueOursStaging means THIS session's move record names the
	// candidate as its `.af-copy-*` staged copy — possibly incomplete.
	ResidueOursStaging
	// ResidueOursRetained means THIS session's own persisted
	// ArchiveReport.RetainedTrees names the candidate: a tree already
	// accounted for as deliberately kept work, adopted and carried forward —
	// never residue, never refused, never dropped.
	ResidueOursRetained
	// ResidueForeignSession means ANOTHER session's durable record or
	// retained-tree report names the candidate. It never blocks this
	// session, and is never adopted or moved by it.
	ResidueForeignSession
)

// ResidueArbiter is the durable-record half of residue arbitration. It folds
// this session's live record and retained-tree report in first (the caller
// always has the instance in hand, so self-attribution never depends on a
// disk read) and lazily enumerates every session row in this AF home only
// when a candidate it could not self-attribute needs a foreign check. An
// enumeration that could not read a store is reported so the caller can
// fail closed naming that store — but only matters when an otherwise
// unverifiable residue candidate actually exists.
type ResidueArbiter struct {
	selfID    string
	selfNamed map[string]ResidueAttribution

	enumerated      bool
	foreignNamed    map[string]bool
	foreignRetained map[string]bool
	enumStores      []string
	enumErr         error
}

// NewResidueArbiter builds the arbiter for the session whose arbitration a
// route is running. The instance's live recovery record and archive report
// supply self-attribution; disk enumeration supplies the foreign half. A nil
// instance is the no-self form af doctor uses: every record-named candidate
// then reports ResidueForeignSession, because it is claimed — just not by a
// session the caller is acting for.
func NewResidueArbiter(i *Instance) *ResidueArbiter {
	a := &ResidueArbiter{selfNamed: make(map[string]ResidueAttribution)}
	if i == nil {
		return a
	}
	a.selfID = i.ID
	if recovery, ok := i.WorktreeRelocationRecovery(); ok {
		if recovery.QuarantinePath != "" {
			a.selfNamed[recovery.QuarantinePath] = ResidueOursQuarantine
		}
		if recovery.StagingPath != "" {
			a.selfNamed[recovery.StagingPath] = ResidueOursStaging
		}
	}
	for _, tree := range i.GetArchiveReport().RetainedTrees {
		if tree.Path != "" {
			a.selfNamed[tree.Path] = ResidueOursRetained
		}
	}
	return a
}

// Classify reports the durable-record attribution for one residue candidate:
// this session's names first, then — only when the candidate is not
// self-attributed — the AF-home-wide enumeration of every other session's
// persisted records and retained trees.
func (a *ResidueArbiter) Classify(candidate string) ResidueAttribution {
	if attr, ok := a.selfNamed[candidate]; ok {
		return attr
	}
	a.enumerate()
	if a.foreignNamed[candidate] || a.foreignRetained[candidate] {
		return ResidueForeignSession
	}
	return ResidueUnattributed
}

// EnumerationProblem reports the unreadable store paths and the composed
// problem when this AF home's session rows could not all be enumerated. The
// caller folds this into a fail-closed refusal ONLY when a residue candidate
// actually exists whose attribution the enumeration could have changed — a
// named-candidate answer never needs the store.
func (a *ResidueArbiter) EnumerationProblem() (stores []string, err error) {
	a.enumerate()
	return a.enumStores, a.enumErr
}

// enumerate loads every repo's instances.json in this AF home and indexes
// the residue paths each session's recovery record and retained-tree report
// names. Rows that cannot be decoded, and repos whose file could not be
// read, are recorded as an incomplete enumeration: attribution falls back
// to "no durable record names it" for their contents while the problem is
// reported to the caller.
func (a *ResidueArbiter) enumerate() {
	if a.enumerated {
		return
	}
	a.enumerated = true
	a.foreignNamed = make(map[string]bool)
	a.foreignRetained = make(map[string]bool)
	rows, skipped, err := config.LoadAllRepoInstancesReportingSkipDetailsSilent()
	if err != nil {
		a.enumErr = fmt.Errorf("cannot enumerate af session records in this home: %w", err)
		return
	}
	if len(skipped) > 0 {
		for _, skip := range skipped {
			if skip.Path != "" {
				a.enumStores = append(a.enumStores, skip.Path)
			} else {
				a.enumStores = append(a.enumStores, "repo "+skip.RepoID)
			}
		}
		a.enumErr = fmt.Errorf("%s; %s",
			config.DescribeRepoInstancesSkips(skipped), config.RepoInstancesSkipRemedy(skipped))
	}
	for repoID, raw := range rows {
		var datas []InstanceData
		if uerr := json.Unmarshal(raw, &datas); uerr != nil {
			if a.enumErr == nil {
				a.enumErr = fmt.Errorf("could not decode a session store in this home: %v", uerr)
			}
			if p, perr := config.RepoInstancesPath(repoID); perr == nil {
				a.enumStores = append(a.enumStores, p)
			} else {
				a.enumStores = append(a.enumStores, "repo "+repoID)
			}
			continue
		}
		for _, d := range datas {
			if d.ID == a.selfID {
				// The live record this arbiter already holds is never
				// behind its durable projection, so self rows are skipped
				// here — classifying own residue goes through selfNamed.
				continue
			}
			if recovery := d.Worktree.RelocationRecovery; recovery != nil {
				if recovery.QuarantinePath != "" {
					a.foreignNamed[recovery.QuarantinePath] = true
				}
				if recovery.StagingPath != "" {
					a.foreignNamed[recovery.StagingPath] = true
				}
			}
			if d.ArchiveReport != nil {
				for _, tree := range d.ArchiveReport.RetainedTrees {
					if tree.Path != "" {
						a.foreignRetained[tree.Path] = true
					}
				}
			}
		}
	}
}
