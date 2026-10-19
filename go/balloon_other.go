//go:build !windows

package main

// balloonNotify is a tray-balloon toast on Windows; other platforms just log.
func balloonNotify(title, text string) { fmt.Println("[toast]", title, "-", text) }
