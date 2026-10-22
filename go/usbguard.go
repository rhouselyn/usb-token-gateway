// usbguard: the whole point of this gateway is that it runs FROM the USB
// stick.  Windows keeps the running image locked, so users yank the stick
// instead of ejecting it -- the process would happily keep serving from
// memory (vault already decrypted) with no way to save anything back.
//
// So: fingerprint the volume the exe lives on at boot and, once that volume
// is gone for a few consecutive probes, flush what we can and exit.  A quick
// unplug/replug inside the grace window keeps the session alive.
package main

import (
	"os"
	"path/filepath"
	"time"

	"usbridge/internal/bridge"
)

const (
	usbGuardTick     = time.Second
	usbGuardMisses   = 5 // ~5s of a missing volume before we shut down
	usbGuardReplugMS = 1200
)

func startUSBGuard(b *bridge.Bridge) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	root := filepath.VolumeName(exe)
	if root == "" {
		return // no volume notion (unusual build layout): stay out of the way
	}
	serial, ok := volumeSerial(root)
	if !ok {
		return // network path / unreadable volume: cannot fingerprint safely
	}
	marker := b.DataDir() // lives on the same volume as the exe
	misses := 0
	for {
		time.Sleep(usbGuardTick)
		cur, okNow := volumeSerial(root)
		if okNow && cur == serial && drivePresent(root) && dirExists(marker) {
			misses = 0
			continue
		}
		misses++
		if misses < usbGuardMisses {
			continue // tolerate a quick replug / transient I/O hiccup
		}
		balloonNotify("U 盘已拔出", "网关已自动退出；插回 U 盘后重新双击 usb-bridge.exe 即可")
		time.Sleep(usbGuardReplugMS * time.Millisecond)
		if a := b.Audit(); a != nil {
			a.Close() // no-op when the drive is gone, frees the handle otherwise
		}
		os.Exit(0)
	}
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
