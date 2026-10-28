//go:build !cgo

package session

// nativeResolverIsSystem: a CGO_ENABLED=0 binary (the static release builds) uses
// Go's own resolver, which never consults NSS modules (mDNS, LDAP, mymachines).
// A name must not be resolved in-process there; see resolvePinnedViaGetent.
const nativeResolverIsSystem = false
