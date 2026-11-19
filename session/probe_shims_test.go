package session

// Probe-only shim: dockerRootScriptPrefix is the PATH pin item 3 splices
// into every root script. Master scripts carry no pin — the constant exists
// here only so the copied tests name the pin master's scripts must NOT
// contain.
const dockerRootScriptPrefix = "export PATH=/usr/sbin:/usr/bin:/sbin:/bin; "

// dockerRootProgramPaths / dockerRootProgramNames are the lists item 3's
// verifier asks after. On master there is no verifier, so these exist only
// for the copied test's assertions.
var dockerRootProgramPaths = []string{"/bin/sh", "/bin/cat", "/usr/bin/stat", "/bin/stat"}
var dockerRootProgramNames = []string{"cat", "chmod", "chown", "find", "git", "id", "mkdir"}
