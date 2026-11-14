package fsprobe

import (
	"sync/atomic"
	"time"
)

// seam is a replaceable syscall a bounded flight runs: the production call,
// or a test's stand-in for a wedged mount. It is atomic because a flight that
// outlives its bound by design (the mount never answers) can still be running,
// and starting a nested flight, after its test restored the seam (#7278 review).
// Callers load it once, in the caller, so a running flight never sees a later
// replacement.
type seam[F any] struct{ fn atomic.Pointer[F] }

func newSeam[F any](fn F) *seam[F] {
	s := &seam[F]{}
	s.fn.Store(&fn)
	return s
}

func (s *seam[F]) load() F { return *s.fn.Load() }

// replace installs fn and returns the function that puts the previous one back.
func (s *seam[F]) replace(fn F) (restore func()) {
	old := s.fn.Swap(&fn)
	return func() { s.fn.Store(old) }
}

// bound is a flight's deadline, atomic for the same reason as seam: a flight
// still running after its test returned reads it when it starts a nested one.
type bound struct{ nanos atomic.Int64 }

func newBound(d time.Duration) *bound {
	b := &bound{}
	b.nanos.Store(int64(d))
	return b
}

func (b *bound) load() time.Duration { return time.Duration(b.nanos.Load()) }

// replace installs d and returns the function that puts the previous one back.
func (b *bound) replace(d time.Duration) (restore func()) {
	old := b.nanos.Swap(int64(d))
	return func() { b.nanos.Store(old) }
}
