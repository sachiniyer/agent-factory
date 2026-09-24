package doctor

import (
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// defaultInRepoUnknownLeaves loads the in-repo config of the repository
// containing the current working directory and returns the [docker]/[ssh]
// leaves it dropped as unknown. Outside a git repo, with no in-repo file, or
// when the file fails to load for another reason, it returns nothing: the
// load error itself is reported by the checks that resolve the repo config.
func defaultInRepoUnknownLeaves() []config.InRepoUnknownLeaf {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	repo, err := config.RepoFromPath(cwd)
	if err != nil {
		return nil
	}
	cfg, _, err := config.LoadInRepoConfig(repo.Root)
	if err != nil {
		return nil
	}
	return cfg.UnknownLeaves()
}

// checkInRepoUnknownLeaves turns each unknown [docker]/[ssh] leaf in the repo's
// checked-in config into a WARN (#4599). The config still loads and the key is
// ignored, so the finding is advisory (problem=false, exit 0) until a later
// release makes it a load error (#4845). A clean file adds no row.
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
		report.Warn(sectionConfig, "in-repo config", detail, remedy, false)
	}
}
