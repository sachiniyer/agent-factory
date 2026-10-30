//go:build windows

package daemon

func freeBytesAt(string) (uint64, bool) { return 0, false }
