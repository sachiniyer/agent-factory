package tmuxguard

func inspectGitHub(args []shellWord) string {
	if len(args) == 0 || !args[0].resolved {
		return unknownShellReason
	}
	command := args[0].literal
	switch command {
	case "--help", "--version", "api", "completion", "status":
		return ""
	case "alias", "browse", "codespace", "extension":
		return unknownShellReason
	}
	if len(args) < 2 || !args[1].resolved || !safeGitHubSubcommand(command+"/"+args[1].literal) {
		// Unknown top-level names are extension executables; unknown nested
		// names may become executable in a future gh release.
		return unknownShellReason
	}
	return ""
}

func safeGitHubSubcommand(path string) bool {
	switch path {
	case "auth/login", "auth/logout", "auth/refresh", "auth/setup-git", "auth/status",
		"auth/switch", "auth/token",
		"cache/delete", "cache/list",
		"config/clear-cache", "config/get", "config/list",
		"gist/create", "gist/delete", "gist/edit", "gist/list", "gist/rename", "gist/view",
		"gpg-key/add", "gpg-key/delete", "gpg-key/list",
		"issue/close", "issue/comment", "issue/create", "issue/delete", "issue/edit",
		"issue/list", "issue/lock", "issue/pin", "issue/reopen", "issue/status",
		"issue/transfer", "issue/unlock", "issue/unpin", "issue/view",
		"label/clone", "label/create", "label/delete", "label/edit", "label/list",
		"org/list",
		"pr/checks", "pr/close", "pr/comment", "pr/create", "pr/diff", "pr/edit",
		"pr/list", "pr/lock", "pr/merge", "pr/ready", "pr/reopen", "pr/review",
		"pr/status", "pr/unlock", "pr/view",
		"project/close", "project/copy", "project/create", "project/delete", "project/edit",
		"project/field-create", "project/field-delete", "project/field-list",
		"project/item-add", "project/item-archive", "project/item-create",
		"project/item-delete", "project/item-edit", "project/item-list", "project/link",
		"project/list", "project/mark-template", "project/unlink", "project/view",
		"release/create", "release/delete", "release/delete-asset", "release/download",
		"release/edit", "release/list", "release/upload", "release/view",
		"repo/archive", "repo/delete", "repo/deploy-key", "repo/edit", "repo/list",
		"repo/rename", "repo/set-default", "repo/unarchive", "repo/view",
		"ruleset/check", "ruleset/list", "ruleset/view",
		"run/cancel", "run/delete", "run/download", "run/list", "run/rerun", "run/view",
		"run/watch",
		"search/code", "search/commits", "search/issues", "search/prs", "search/repos",
		"secret/delete", "secret/list", "secret/set",
		"ssh-key/add", "ssh-key/delete", "ssh-key/list",
		"variable/delete", "variable/list", "variable/set",
		"workflow/disable", "workflow/enable", "workflow/list", "workflow/run", "workflow/view":
		return true
	default:
		return false
	}
}
