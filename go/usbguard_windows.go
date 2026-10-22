//go:build windows

package main

import (
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32Vol               = syscall.NewLazyDLL("kernel32.dll")
	procGetVolumeInformationW = kernel32Vol.NewProc("GetVolumeInformationW")
	procGetLogicalDrives      = kernel32Vol.NewProc("GetLogicalDrives")
)

// volumeSerial fingerprints the volume behind root (the USB stick's identity).
// A different serial = a different device reused the same drive letter.
func volumeSerial(root string) (uint32, bool) {
	if root == "" {
		return 0, false
	}
	p, err := syscall.UTF16PtrFromString(withTrailingSlash(root))
	if err != nil {
		return 0, false
	}
	var serial, maxComp, flags uint32
	fsName := make([]uint16, 64)
	r, _, _ := procGetVolumeInformationW.Call(
		uintptr(unsafe.Pointer(p)), 0, 0,
		uintptr(unsafe.Pointer(&serial)),
		uintptr(unsafe.Pointer(&maxComp)),
		uintptr(unsafe.Pointer(&flags)),
		uintptr(unsafe.Pointer(&fsName[0])),
		uintptr(len(fsName)),
	)
	if r == 0 {
		return 0, false
	}
	return serial, true
}

// drivePresent is a cheap, I/O-free check: did the drive letter disappear?
func drivePresent(root string) bool {
	r := strings.ToUpper(strings.TrimRight(root, `\/`))
	if len(r) != 2 || r[1] != ':' {
		return true // UNC or relative path: no drive-letter bit to test
	}
	mask, _, _ := procGetLogicalDrives.Call()
	if mask == 0 {
		return true // query failed: don't turn that into a false "unplugged"
	}
	i := int(r[0] - 'A')
	if i < 0 || i > 25 {
		return true
	}
	return mask&(1<<uint(i)) != 0
}

func withTrailingSlash(p string) string {
	if strings.HasSuffix(p, `\`) || strings.HasSuffix(p, "/") {
		return p
	}
	return p + `\`
}
