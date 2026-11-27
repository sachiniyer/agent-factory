package doctor

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

const checkRelocationResidue = "relocation-residue"

// checkAFRelocationResidue reports `.af-source-<32hex>`/`.af-copy-<32hex>`
// residue that NO durable af record names (#7819). Residue a session's own
// move record or retained-tree report claims is already arbitrated by that
// session — it blocks its rebuild or carries forward at adopt time — so it
// needs no operator and produces no row. Residue nothing names is the
// pre-records case: a crash before af kept move records can have stranded a
// complete worktree under one of these names, and only the operator can say
// whether the contents matter. Each such path gets a WARN row with the
// manual remediation; af never removes it itself.
//
// The scan reads the parents of every path a session record keeps — the
// recorded worktree path, a recovery record's alternate and residue
// destinations, and every retained-tree path — because a cross-device move
// only ever leaves residue beside a path af wrote down.
func checkAFRelocationResidue(ctx *scanContext, report *Report) {
	all, skipped, err := config.LoadAllRepoInstancesReportingSkipDetails()
	if err != nil {
		report.Warn(sectionProcesses, checkRelocationResidue,
			"could not enumerate session records to scan for relocation residue: "+oneLineErr(err),
			"repair the af session store, then rerun `af doctor`", false)
		report.markIncomplete(checkRelocationResidue)
		return
	}
	var datas []session.InstanceData
	for _, raw := range all {
		var list []session.InstanceData
		if uerr := json.Unmarshal(raw, &list); uerr != nil {
			report.Warn(sectionProcesses, checkRelocationResidue,
				"could not decode a session store while scanning for relocation residue: "+oneLineErr(uerr),
				"repair the af session store, then rerun `af doctor`", false)
			report.markIncomplete(checkRelocationResidue)
			continue
		}
		datas = append(datas, list...)
	}
	if len(skipped) > 0 {
		report.Warn(sectionProcesses, checkRelocationResidue,
			config.DescribeRepoInstancesSkips(skipped)+", so residue those sessions' records would name cannot be told from unclaimed residue",
			config.RepoInstancesSkipRemedy(skipped)+", then rerun `af doctor`", false)
		report.markIncomplete(checkRelocationResidue)
	}

	dirs := map[string]bool{}
	recordDir := func(path string) {
		if path != "" {
			dirs[filepath.Dir(path)] = true
		}
	}
	for i := range datas {
		d := &datas[i]
		recordDir(d.Path)
		recordDir(d.Worktree.WorktreePath)
		if recovery := d.Worktree.RelocationRecovery; recovery != nil {
			recordDir(recovery.AlternatePath)
			recordDir(recovery.QuarantinePath)
			recordDir(recovery.StagingPath)
		}
		if d.ArchiveReport != nil {
			for _, tree := range d.ArchiveReport.RetainedTrees {
				recordDir(tree.Path)
			}
		}
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Strings(ordered)

	// The no-self arbiter answers "claimed" for any session's record or
	// report — claimed residue is that session's arbitration problem, not a
	// doctor finding. Only ResidueUnattributed candidates surface here.
	arbiter := session.NewResidueArbiter(nil)
	for _, dir := range ordered {
		residue, lerr := sessiongit.ListAFRelocationResidue(dir)
		if lerr != nil {
			report.Warn(sectionProcesses, checkRelocationResidue,
				fmt.Sprintf("could not scan %s for af relocation residue: %s", dir, oneLineErr(lerr)),
				"make the directory readable, then rerun `af doctor`", false)
			report.markIncomplete(checkRelocationResidue)
			continue
		}
		for _, candidate := range residue {
			if arbiter.Classify(candidate) != session.ResidueUnattributed {
				continue
			}
			report.Warn(sectionProcesses, checkRelocationResidue,
				fmt.Sprintf("unclaimed af relocation residue is present at %s — no af session's durable move record or retained-tree report names it, so it may be a complete worktree stranded by an interrupted archive that predates af's move records", candidate),
				fmt.Sprintf("inspect %s; if it holds work you need, move it somewhere safe, then remove it from %s — do not move it into a session's recorded path unless you have proven it is that session's work", candidate, dir),
				true)
		}
	}
	if _, enumErr := arbiter.EnumerationProblem(); enumErr != nil {
		report.Warn(sectionProcesses, checkRelocationResidue,
			fmt.Sprintf("af could not read every session record in this home (%v), so some residue above may in fact be claimed by a record af could not read", enumErr),
			"repair the named session store, then rerun `af doctor`", false)
		report.markIncomplete(checkRelocationResidue)
	}
}
