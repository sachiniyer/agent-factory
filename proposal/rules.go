package proposal

import "fmt"

// Rules is the working contract that start and attach print for the agent.
func Rules(slug, path string) string {
	return fmt.Sprintf(`You are working on proposal %[1]s. Follow these rules until the user tells you to stop:
1. The docs are at %[2]s. Edit them with your normal file tools.
2. Before you edit, run "af proposal changes" and read what the user changed. The user's edits can also arrive as a prompt that starts with "Proposal %[1]s:". That prompt already holds the edits, so after it you do not need to run "af proposal changes" first.
3. After each round of edits, run "af proposal checkpoint".
4. Put questions for the user in "> [!question]" callouts, in the doc they belong to. Then stop and tell the user.
5. Do each "> [!todo]" callout, then delete it.
6. Do not change a "> [!decision]" callout without asking the user.
7. To add a doc, run "af proposal add-doc %[1]s <name>".
8. If you lose track of the proposal, run "af proposal attach".
9. When the user asks you to make backlog items from the proposal, or to update them, write them in one batch with "af backlog add --file -" and JSON on stdin: {"proposal": "%[1]s", "items": [...]}. Every item needs a "title" and a "key". Make one parent item for the proposal, with key "proposal" and "sequential": true, and one child per row of the PR stack in steps.md, in stack order, with "parent": "proposal" and a key "pr-<name>" that names the PR in a few lowercase-dashed words, such as "pr-item-store", never its row number. Give each child a body with what the PR does and the proposal path, %[2]s. Every item is a draft: do not queue, start, drop or delete items. To update, first read this proposal's items with "af backlog list --all --json" and reuse each item's source key for the PR it stands for, even if its row moved or was renamed: an item with the same proposal and key is updated, not added again. A batch does not reorder items that already exist and adds new ones last, so after it compare the order of the children in "af backlog list" with the stack and fix it with "af backlog move <id> --before <id>" or "--after <id>". The parent is sequential, so its children run in that order; use "waits_on" only for a dependency the order does not give. Then tell the user the item ids it printed, and name any existing item of this proposal that no longer matches a row.`, slug, path)
}
