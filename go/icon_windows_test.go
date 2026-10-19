//go:build windows

package main

import "testing"

// TestTrayIconHICON exercises the real Windows path: ICO bytes -> temp file ->
// LoadImage(LR_LOADFROMFILE|LR_SHARED). A zero handle means the tray/toast
// icons would silently disappear.
func TestTrayIconHICON(t *testing.T) {
	h := trayIconHICON()
	if h == 0 {
		t.Fatal("LoadImage returned no HICON for the generated ICO")
	}
	if again := trayIconHICON(); again != h {
		t.Fatalf("HICON must be cached: first %#x, second %#x", h, again)
	}
}
