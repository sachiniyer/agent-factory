package doctor

import (
	"errors"
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// defaultInRepoUnknownLeaves loads the in-repo config of the repository
// containing the current working directory and returns the [docker]/[ssh]
// leaves that made the load fail (#4845). Outside a git repo, with no in-repo
// file, or when the file fails to load for another reason, it returns nothing:
// any other load error is reported by the checks that resolve the repo config.
func defaultInRepoUnknownLeaves() []config.InRepoUnknownLeaf {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	repo, err := config.RepoFromPath(cwd)
	if err != nil {
		return nil
	}
	_, _, err = config.LoadInRepoConfig(repo.Root)
	var unknown *config.InRepoUnknownLeavesError
	if errors.As(err, &unknown) {
		return unknown.Leaves
	}
	return nil
}

// checkInRepoUnknownLeaves reports each unknown [docker]/[ssh] leaf in the repo's
// checked-in config as a FAIL. Since #4845 such a leaf fails the config load, so
// every af command in the repo errors until it is fixed; the finding names the
// file and key and gives the rename. A clean file adds no row.
func checkInRepoUnknownLeaves(ctx *scanContext, report *Report) {
	resolve := ctx.opts.inRepoUnknownLeaves
	if resolve == nil {
		resolve = defaultInRepoUnknownLeaves
	}
	for _, leaf := range resolve() {
		remedy := "remove the key from " + leaf.Path + " or correct its spelling"
		if leaf.Suggestion != "" {
			remedy = "rename " + leaf.Table + "." + leaf.Key + " to " + leaf.Table + "." + leaf.Suggestion + " in " + leaf.Path
		}
		// The row is already named "in-repo config"; don't say it twice.
		detail := strings.TrimPrefix(leaf.Message, "in-repo config ")
		report.Fail(sectionConfig, "in-repo config", detail, remedy)
	}
}
