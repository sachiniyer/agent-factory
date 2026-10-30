//go:build windows

package daemon

func freeBytesAt(string) (uint64, uint64, bool) { return 0, 0, false }
