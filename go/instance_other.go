//go:build !windows

package main

// killOldInstances / bridgeSquattingPID are Windows-only behaviors
// (tray/session model); other platforms use conventional daemon management.
func killOldInstances() int          { return 0 }
func bridgeSquattingPID(int) int     { return 0 }
