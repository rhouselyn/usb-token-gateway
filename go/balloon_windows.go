//go:build windows

package main

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	user32  = syscall.NewLazyDLL("user32.dll")
	shell32 = syscall.NewLazyDLL("shell32.dll")

	procCreateWindowExW = user32.NewProc("CreateWindowExW")
	procDestroyWindow   = user32.NewProc("DestroyWindow")
	procLoadIconW       = user32.NewProc("LoadIconW")
	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
)

const (
	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifState   = 0x8
	nifInfo    = 0x10

	nisHidden = 0x1
	niifInfo  = 0x1
	idiInfo   = 32516 // IDI_INFORMATION
)

type notifyIconDataW struct {
	cbSize           uint32
	hWnd             syscall.Handle
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            syscall.Handle
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     syscall.Handle
}

// balloonNotify shows the classic tray balloon toast (bottom-right, ~6s).
// Tray icon used for it is NIS_HIDDEN, so no extra icon appears.
func balloonNotify(title, text string) {
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		cls := utf16.Encode([]rune("STATIC\x00"))
		hwnd, _, err := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(&cls[0])), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
		if hwnd == 0 {
			fmt.Println("[tray balloon] cannot create helper window:", err)
			return
		}
		defer procDestroyWindow.Call(hwnd)

		// brand icon for the toast (falls back to the system info icon)
		icon, _, _ := procLoadIconW.Call(0, uintptr(idiInfo))
		if h := trayIconHICON(); h != 0 {
			icon = h
		}

		nid := notifyIconDataW{
			cbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
			hWnd:             syscall.Handle(hwnd),
			uID:              0x51A,
			uFlags:           nifMessage | nifIcon | nifState,
			uCallbackMessage: 0x8300,
			hIcon:            syscall.Handle(icon),
			hBalloonIcon:     syscall.Handle(icon),
			dwState:          nisHidden,
			dwStateMask:      nisHidden,
		}
		copy(nid.szTip[:], truncate16("USB Bridge", 127))
		if r, _, _ := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); r == 0 {
			fmt.Println("[tray balloon] NIM_ADD failed")
			return
		}
		defer procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))

		nid.uFlags = nifInfo
		nid.dwInfoFlags = niifInfo
		copy(nid.szInfo[:], truncate16(text, 255))
		copy(nid.szInfoTitle[:], truncate16(title, 63))
		if r, _, _ := procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid))); r == 0 {
			fmt.Println("[tray balloon] toast suppressed (focus assist?)")
		}

		time.Sleep(6 * time.Second)
	}()
}

func truncate16(s string, max int) []uint16 {
	u := utf16.Encode([]rune(s))
	if len(u) > max {
		u = u[:max]
	}
	return append(u, 0)
}
