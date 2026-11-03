#!/bin/sh
# Example af setup_commands entry (#6667). Runs on the DAEMON'S machine, in the
# session's source checkout, after af has provisioned the session's workspace
# and established its transport — and before post_worktree_commands and the
# agent start. Wire it in .agent-factory/config.toml:
#
#   setup_commands = ["./examples/hooks/session-setup.sh"]
#
# env:  AF_SETUP_SESSION_ID  session id
#       AF_SETUP_SESSION     session title
#       AF_SETUP_SOURCE      source checkout on the daemon (the script's cwd)
#       AF_SETUP_DEST        payload staging dir — files left here are copied
#                            into the fresh worktree before hooks run. LOCAL
#                            path for a local session; a path ON THE SESSION'S
#                            HOST for a remote one with an ssh route (use the
#                            route below); a daemon-local path af transports
#                            itself when no AF_SSH route is exported (docker)
#       AF_SETUP_MODE        "local" | "remote"
#       AF_SETUP_HOST        the picked host name, when the session has one
#       AF_SSH               ssh command WITHOUT the destination (options only)
#                            — the form `rsync -e` wants. Remote sessions on an
#                            af-composed transport only.
#       AF_SSH_DEST          the destination (user@host or an ssh alias)
#       AF_SSH_FULL          the complete command incl. destination. For a
#                            free-form sandbox.ssh transport AF_SSH and
#                            AF_SSH_DEST are EMPTY and only this is set.
#
# The script is operator-controlled and NOT confined by af on the remote host
# — but this example writes only into AF_SETUP_DEST and the host's own HOME.
set -eu

# AF_SSH and AF_SSH_FULL are SHELL command lines: an option may carry quotes
# (-o "ProxyCommand=nc -x proxy:8080 %h %p"). Expanding them unquoted splits on
# every space and ignores the quotes, so run them through sh -c with the
# remaining arguments forwarded as positional parameters — the same way af
# invokes the command itself.
af_ssh()      { sh -c "$AF_SSH \"\$@\"" af-ssh "$@"; }
af_ssh_full() { sh -c "$AF_SSH_FULL \"\$@\"" af-ssh-full "$@"; }

# What this session needs beyond the clone: .env* files (loop 1), plus the
# selected uncommitted files listed below (loop 2). Edit the list for your
# repo — .env.local already rides the .env* glob, so it is not repeated here.
WANTED_FILES="config/dev.local.yml"

# copy_in SRC copies one source file into the payload staging dir, preserving
# its path below the checkout root so the overlay lands it in the same place.
# When no AF_SSH route is exported (local sessions, and off-box runtimes with
# no ssh transport such as docker), AF_SETUP_DEST is a daemon-local path af
# transports itself — plain file operations are the whole contract.
copy_in() {
    rel=$1
    src=$AF_SETUP_SOURCE/$rel
    [ -f "$src" ] || return 0
    if [ -n "${AF_SSH_DEST:-}" ] && [ -n "${AF_SSH:-}" ]; then
        af_ssh "$AF_SSH_DEST" "mkdir -p '$AF_SETUP_DEST/$(dirname "$rel")'"
        rsync -a -e "$AF_SSH" "$src" "$AF_SSH_DEST:$AF_SETUP_DEST/$rel"
    elif [ -n "${AF_SSH_FULL:-}" ]; then
        # Free-form transport: rsync -e cannot be used, so pipe tar over
        # AF_SSH_FULL the way move_cmd documents.
        tar -C "$AF_SETUP_SOURCE" -cf - "$rel" | af_ssh_full "tar -C '$AF_SETUP_DEST' -xf -"
    else
        mkdir -p "$AF_SETUP_DEST/$(dirname "$rel")"
        cp -p "$src" "$AF_SETUP_DEST/$rel"
    fi
}

# 1. .env* files from the checkout root — but only UNTRACKED ones. A
# checked-in .env.example is already in the worktree; staging it again makes
# the overlay hit its checked-in-path collision guard and skip the real
# payload behind it. git ls-files --error-unmatch names tracked files.
for f in "$AF_SETUP_SOURCE"/.env*; do
    [ -f "$f" ] || continue
    git -C "$AF_SETUP_SOURCE" ls-files --error-unmatch -- "$(basename "$f")" >/dev/null 2>&1 && continue
    copy_in "$(basename "$f")"
done

# 2. Selected uncommitted files an agent needs but git does not carry.
for rel in $WANTED_FILES; do
    copy_in "$rel"
done

# 3. A command run on the host — the generic half of the hook. Local sessions
# run it here; sessions with an ssh route run it through the session's own
# connection. An off-box runtime with no ssh route (docker) exposes no host
# command channel, so there is nothing to run it through.
if [ "$AF_SETUP_MODE" = "local" ]; then
    mkdir -p "$HOME/.cache/myapp"
elif [ -n "${AF_SSH_DEST:-}" ] && [ -n "${AF_SSH:-}" ]; then
    af_ssh "$AF_SSH_DEST" "mkdir -p \"\$HOME/.cache/myapp\""
elif [ -n "${AF_SSH_FULL:-}" ]; then
    af_ssh_full "mkdir -p \"\$HOME/.cache/myapp\""
fi
