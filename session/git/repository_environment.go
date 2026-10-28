package git

import "strings"

// repositoryPathEnvironment preserves the caller's credential/runtime policy
// while removing Git's repository-local environment. A command naming a repo by
// path must not let inherited variables silently select a different repository,
// even when an operator allowed those variables through the hook secret filter.
//
// These are the names reported by git rev-parse --local-env-vars in any Git
// release that has the flag (1.7.1 onward), not only the current one: a host
// runs whatever Git it has, and older releases list names newer ones dropped.
// Git 2.11 through 2.39 list GIT_INTERNAL_SUPER_PREFIX, a path from a
// superproject down to this repository's root (added by git.git 74866d757935,
// removed in 2.40 by 4002ec3dcf0f). Keep this list explicit rather than
// spawning another Git process for every bounded command; the compatibility
// test checks it against the installed Git. Indexed config entries accompany
// GIT_CONFIG_COUNT and must not cross the boundary either. PATH, HOME, global
// Git config, and unrelated hook variables remain unchanged.
func repositoryPathEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
			"GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE",
			"GIT_GRAFT_FILE", "GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE",
			"GIT_PREFIX", "GIT_INTERNAL_SUPER_PREFIX", "GIT_SHALLOW_FILE", "GIT_COMMON_DIR":
			continue
		}
		if strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			continue
		}
		result = append(result, entry)
	}
	return result
}
