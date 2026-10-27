#!/bin/sh
# Example af move script (W7.9, #5571). Runs on the daemon box after af has
# seeded the sandbox workspace from the session's branch + uncommitted work.
# Copy what git does not carry: .gitignored files such as .env*, a credentials
# file, a cache directory. Exit 0 to continue the move; non-zero aborts it and
# the local session keeps running untouched.
#
# stdin : JSON — session_id, title, branch, source_worktree, host,
#         dest_workspace, account (optional)
# env   : AF_SSH — the exact ssh command af uses for this host
set -eu

spec=$(cat)
field() { printf '%s' "$spec" | python3 -c 'import json,sys; print(json.load(sys.stdin)["'"$1"'"])'; }
src=$(field source_worktree)
dest=$(field dest_workspace)
host=$(field host)

# 1. .env files from the worktree root.
for f in "$src"/.env*; do
  [ -f "$f" ] || continue
  rsync -a -e "$AF_SSH" "$f" "$host:$dest/"
done

# 2. A credentials file outside the repo (adjust to taste).
if [ -f "$HOME/.config/myservice/credentials.json" ]; then
  $AF_SSH "$host" 'mkdir -p ~/.config/myservice && chmod 700 ~/.config/myservice'
  rsync -a -e "$AF_SSH" "$HOME/.config/myservice/credentials.json" "$host:.config/myservice/"
fi

# 3. A build cache so the first build on the sandbox is warm.
[ -d "$src/.cache" ] && rsync -a -e "$AF_SSH" "$src/.cache/" "$host:$dest/.cache/"
exit 0
