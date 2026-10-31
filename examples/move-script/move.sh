#!/bin/sh
# Example af move script (W7.9, #5571). Runs on the daemon box after af has
# seeded the sandbox workspace from the session's branch + uncommitted work.
# Copy what git does not carry: .gitignored files such as .env*, a credentials
# file, a cache directory. Exit 0 to continue the move; non-zero aborts it and
# the local session keeps running untouched.
#
# stdin : JSON — session_id, title, branch, source_worktree, host,
#         dest_workspace, account (optional)
# env   : AF_SSH       ssh command WITHOUT the destination (options only)
#         AF_SSH_DEST  the destination (user@host or an ssh alias)
#         AF_SSH_FULL  the complete command incl. destination. For a free-form
#                      sandbox.ssh target AF_SSH and AF_SSH_DEST are EMPTY and
#                      only this is set — see the fallback at the bottom.
set -eu

# AF_SSH and AF_SSH_FULL are SHELL command lines: an option may carry quotes
# (-o "ProxyCommand=nc -X connect -x proxy:8080 %h %p"). Expanding them unquoted
# splits on every space and ignores the quotes, so run them through sh -c with the
# remaining arguments forwarded as positional parameters, which is how af runs the
# same command itself.
af_ssh()      { sh -c "$AF_SSH \"\$@\"" af-ssh "$@"; }
af_ssh_full() { sh -c "$AF_SSH_FULL \"\$@\"" af-ssh-full "$@"; }

spec=$(cat)
field() { printf '%s' "$spec" | python3 -c 'import json,sys; print(json.load(sys.stdin)["'"$1"'"])'; }
src=$(field source_worktree)
dest=$(field dest_workspace)
if [ -z "${AF_SSH_DEST:-}" ]; then
  # Free-form target: rsync -e cannot be used, so pipe tar over AF_SSH_FULL.
  for f in "$src"/.env*; do
    [ -f "$f" ] || continue
    tar -C "$src" -c "$(basename "$f")" | af_ssh_full "tar -C '$dest' -x"
  done
  exit 0
fi

# 1. .env files from the worktree root.
for f in "$src"/.env*; do
  [ -f "$f" ] || continue
  rsync -a -e "$AF_SSH" "$f" "$AF_SSH_DEST:$dest/"
done

# 2. A credentials file outside the repo (adjust to taste).
if [ -f "$HOME/.config/myservice/credentials.json" ]; then
  af_ssh "$AF_SSH_DEST" 'mkdir -p ~/.config/myservice && chmod 700 ~/.config/myservice'
  rsync -a -e "$AF_SSH" "$HOME/.config/myservice/credentials.json" "$AF_SSH_DEST:.config/myservice/"
fi

# 3. A build cache so the first build on the sandbox is warm.
[ -d "$src/.cache" ] && rsync -a -e "$AF_SSH" "$src/.cache/" "$AF_SSH_DEST:$dest/.cache/"
exit 0
