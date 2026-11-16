package systemdunit

import "time"

// BoundChildStopTimeoutDuration is how long systemd waits after SIGTERM before
// it SIGKILLs a scope bound to the daemon's unit. NewBoundChildCommand passes
// it to systemd-run as TimeoutStopSec= (BoundChildStopTimeout on Linux, pinned
// to this value by a test).
//
// Bound scopes are After= the daemon's unit, so on a stop systemd holds the
// daemon's own SIGTERM until every bound scope has stopped. This is therefore
// also the longest that the daemon's stop signal can trail a bound child's
// SIGTERM. The daemon's watch supervisor sizes its stop excuse from it, and
// pins that relation at compile time (#7404). It is declared on every platform
// so the pin compiles everywhere.
const BoundChildStopTimeoutDuration = 4 * time.Second
