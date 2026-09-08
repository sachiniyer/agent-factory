package apiproto

import "time"

// OperationLockTimeout is the daemon admission bound for lifecycle mutations.
// Snapshot projects this value so browser uncertainty fences wait out the same
// interval instead of duplicating the duration in TypeScript.
const OperationLockTimeout = 30 * time.Second
