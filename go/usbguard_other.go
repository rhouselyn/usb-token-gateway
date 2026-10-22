//go:build !windows

package main

import "os"

// Non-Windows dev builds: fall back to "is the mount point still there?".
func volumeSerial(root string) (uint32, bool) {
	if _, err := os.Stat(root); err != nil {
		return 0, false
	}
	return 1, true
}

func drivePresent(string) bool { return true }
