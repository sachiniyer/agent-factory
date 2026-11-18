//go:build !linux && !darwin

package proctree

func unixSockets(int) ([]UnixSocket, error) { return nil, ErrUnsupportedPlatform }

func unixListenerPathsByTable(int) ([]string, error) { return nil, ErrUnsupportedPlatform }
