//go:build windows

package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

const (
	imageIcon      = 1
	lrLoadFromFile = 0x0010
	lrDefaultSize  = 0x0040
	lrShared       = 0x8000
)

var procLoadImageW = user32.NewProc("LoadImageW")

var (
	iconLoadOnce sync.Once
	iconHICON    uintptr
)

// trayIconHICON returns an HICON for our multi-size ICO (shared, loaded once).
// It goes through a temp .ico file because LoadImage(LR_LOADFROMFILE) is the
// reliable way to get Windows to pick the right size out of a PNG-based ICO;
// the balloon toasts reuse the same handle so tray + toast share one artwork.
func trayIconHICON() uintptr {
	iconLoadOnce.Do(func() {
		sum := md5.Sum(iconPNG)
		path := filepath.Join(os.TempDir(), "usb-bridge-"+hex.EncodeToString(sum[:6])+".ico")
		if _, err := os.Stat(path); err != nil {
			if err := os.WriteFile(path, iconPNG, 0o644); err != nil {
				fmt.Println("[icon] cannot write temp ico:", err)
				return
			}
		}
		p, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			return
		}
		h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(p)),
			imageIcon, 0, 0, lrLoadFromFile|lrDefaultSize|lrShared)
		if h == 0 {
			fmt.Println("[icon] LoadImage failed for", path)
			return
		}
		iconHICON = h
	})
	return iconHICON
}
