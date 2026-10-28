//go:build cgo

package session

// nativeResolverIsSystem: with cgo, net.Dialer resolves through getaddrinfo, the
// same NSS-aware path ssh uses, so a dial here and ssh agree on a name's answers.
const nativeResolverIsSystem = true
