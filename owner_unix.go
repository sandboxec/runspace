//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether path, with symlinks followed, is owned
// by the effective user. Unlike git, it makes no exception for sudo's
// SUDO_UID, so a repository used through sudo needs -R.
func ownedByCurrentUser(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}

	return int64(stat.Uid) == int64(os.Geteuid()), nil
}
