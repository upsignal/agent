//go:build !linux

package collector

import "errors"

// The agent only targets Linux; this stub lets the package build and test on developer machines.
func statfs(string) (fsStats, error) {
	return fsStats{}, errors.New("filesystem statistics are only supported on Linux")
}
