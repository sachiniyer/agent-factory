package config

import "golang.org/x/sys/unix"

// dirSearchOnlyFlag opens a directory for use as a dirfd WITHOUT requiring read
// permission on it. openat, renameat, unlinkat and fstatat all accept such a
// descriptor; only fsync does not, which costs crash durability and nothing
// else — and a path-based writer could not fsync that directory either, so the
// arrangement loses nothing it used to have (#3697 review).
//
// Linux spells it O_PATH. Platforms without an equivalent get 0 and the caller
// refuses with an actionable error instead of silently writing unpinned.
const dirSearchOnlyFlag = unix.O_PATH
