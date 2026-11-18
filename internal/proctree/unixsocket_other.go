//go:build !linux && !darwin

package proctree

func unixListenerPaths(int) ([]string, error) { return nil, ErrUnsupportedPlatform }
