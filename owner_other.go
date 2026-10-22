//go:build !unix && !windows

package main

// ownedByCurrentUser refuses every path on a system whose file ownership
// originRepo cannot check, so a repository there needs -R.
func ownedByCurrentUser(string) (bool, error) { return false, nil }
