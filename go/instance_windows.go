//go:build windows

package main

// killOldInstances clears instances of THIS exact exe image (same basename)
// only: re-double-clicking usb-bridge.exe takes over previous usb-bridge.exe
// copies.  Different-family images (usbgw.exe dev builds, python prototype)
// are NOT killed here -- python prototypes get taken over later only through
// bridgeSquattingPID when they actually hold our port.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	kernel32Inst = syscall.NewLazyDLL("kernel32.dll")

	procCreateToolhelp32Snapshot = kernel32Inst.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW          = kernel32Inst.NewProc("Process32FirstW")
	procProcess32NextW           = kernel32Inst.NewProc("Process32NextW")
	procOpenProcess              = kernel32Inst.NewProc("OpenProcess")
	procTerminateProcess         = kernel32Inst.NewProc("TerminateProcess")
	procCloseHandle              = kernel32Inst.NewProc("CloseHandle")
)

const (
	th32csSnapProcess = 0x2
	processTerminate  = 0x0001
)

type processEntry32W struct {
	dwSize              uint32
	cntUsage            uint32
	th32ProcessID       uint32
	th32DefaultHeapID   uintptr
	th32ModuleID        uint32
	cntThreads          uint32
	th32ParentProcessID uint32
	pcPriClassBase      int32
	dwFlags             uint32
	szExeFile           [260]uint16 // fixed-length, zero-terminated WE string
}

func exeBaseName() string {
	return strings.ToLower(strings.TrimSpace(filepathBase(os.Args[0])))
}

func filepathBase(p string) string {
	i := strings.LastIndexByte(p, '\\')
	if i >= 0 && i+1 < len(p) {
		return p[i+1:]
	}
	i = strings.LastIndexByte(p, '/')
	if i >= 0 && i+1 < len(p) {
		return p[i+1:]
	}
	return p
}

func killOldInstances() int {
	killed := 0
	selfName := exeBaseName() // same-image only: usb-bridge.exe replaces usb-bridge.exe

	snap, _, _ := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || snap == ^uintptr(0) { // INVALID_HANDLE_VALUE
		return 0
	}
	defer procCloseHandle.Call(snap)

	pe := &processEntry32W{dwSize: uint32(unsafe.Sizeof(processEntry32W{}))}
	if r, _, _ := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(pe))); r == 0 {
		return 0
	}
	for {
		name := strings.ToLower(utf16ToStr(pe.szExeFile[:]))
		if name == selfName {
			if pid := int(pe.th32ProcessID); pid > 0 && pid != os.Getpid() {
				if terminatePID(pid) {
					killed++
					fmt.Printf("[bridge] took down old instance pid %d (%s)\n", pid, name)
				}
			}
		}
		if r, _, _ := procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(pe))); r == 0 {
			break
		}
	}
	if killed > 0 {
		time.Sleep(400 * time.Millisecond) // let sockets actually close
	}
	return killed
}

func containsName(names []string, n string) bool {
	for _, x := range names {
		if x == n {
			return true
		}
	}
	return false
}

func utf16ToStr(u []uint16) string {
	for i, c := range u {
		if c == 0 {
			u = u[:i]
			break
		}
	}
	runes := utf16.Decode(u)
	return string(runes)
}

func terminatePID(pid int) bool {
	const invalidHandle = ^uintptr(0)
	h, _, _ := procOpenProcess.Call(processTerminate, 0, uintptr(pid))
	if h == 0 || h == invalidHandle {
		return false
	}
	defer procCloseHandle.Call(h)
	r, _, _ := procTerminateProcess.Call(h, 1)
	return r != 0
}

/* --------------------------- port squatter identification --------------------------- */

var (
	iphlpapi                = syscall.NewLazyDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

const (
	afInet                   = 2
	tcpTableOwnerPidListener = 3
	insufficientBuffer       = 122
	processQueryLimited      = 0x0400
)

type tcpRowOwnerPid struct {
	dwState      uint32
	dwLocalAddr  uint32
	dwLocalPort  uint32 // network byte order
	dwRemoteAddr uint32
	dwRemotePort uint32
	dwOwningPid  uint32
}

// listenerPidForPort returns the pid owning a TCP LISTEN socket on this port
// (any local interface), or 0.  NOTE: on this Windows build the NULL-buffer
// probe returns ERROR_INVALID_PARAMETER(87), so we start from a 64KB buffer
// and only realloc on 122 (insufficient).  dwLocalPort comes back in HOST
// byte order (verified empirically: raw 0x3D04 == port 15620).
func listenerPidForPort(port int) int {
	buf := make([]byte, 64*1024)
	for attempt := 0; attempt < 3; attempt++ {
		size := uint32(len(buf))
		r, _, _ := procGetExtendedTcpTable.Call(
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0,
			afInet, tcpTableOwnerPidListener, 0)
		if r == uintptr(insufficientBuffer) && size > uint32(len(buf)) {
			buf = make([]byte, size) // table grew -- retry with exact size
			continue
		}
		if r != 0 {
			return 0
		}
		n := int(*(*uint32)(unsafe.Pointer(&buf[0])))
		sizeofRow := int(unsafe.Sizeof(tcpRowOwnerPid{}))
		for i := 0; i < n; i++ {
			off := 4 + i*sizeofRow
			if off+sizeofRow > len(buf) {
				break
			}
			row := (*tcpRowOwnerPid)(unsafe.Pointer(&buf[off]))
			// dwLocalPort is the low 16 bits in NETWORK byte order
			// (verified: raw 0xFD22 == port 8957; raw 0x3D0D == 3389/RDP)
			v := row.dwLocalPort
			hostPort := uint32(v&0xFF)<<8 | uint32(v>>8&0xFF)
			if int(hostPort) == port {
				return int(row.dwOwningPid)
			}
		}
		return 0
	}
	return 0
}

// 进程快照辅助：pid -> 镜像名
func imageNameForPID(target int) string {
	if target <= 0 {
		return ""
	}
	snap, _, _ := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || snap == ^uintptr(0) {
		return ""
	}
	defer procCloseHandle.Call(snap)
	pe := &processEntry32W{dwSize: uint32(unsafe.Sizeof(processEntry32W{}))}
	if r, _, _ := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(pe))); r == 0 {
		return ""
	}
	for {
		if int(pe.th32ProcessID) == target {
			return strings.ToLower(utf16ToStr(pe.szExeFile[:]))
		}
		if r, _, _ := procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(pe))); r == 0 {
			return ""
		}
	}
}

// bridgeSquattingPID: is the process listening on this port one of OUR
// gateways (python prototype healthz {"ok":true,"started":...} or any Go
// build with "bind_lan")?  Returns its pid, or 0 for an unknown program
// (never killed).
func bridgeSquattingPID(port int) int {
	// Proxy: nil -- must NEVER honor HTTP_PROXY for our own 127.0.0.1 probe
	// (proxy env vars are common and would break the squatter detection).
	client := &http.Client{Timeout: 800 * time.Millisecond, Transport: &http.Transport{}}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		fmt.Printf("[squat-dbg] healthz fetch err: %v\n", err)
		return 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	s := string(body)
	ours := strings.Contains(s, "bind_lan") || // any Go gateway healthz
		(strings.Contains(s, "started") && strings.Contains(s, "\"ok\"")) // python prototype
	if !ours {
		return 0
	}
	pid := listenerPidForPort(port)
	if pid == 0 || pid == os.Getpid() {
		return 0
	}
	name := imageNameForPID(pid)
	known := containsName([]string{"python.exe", "pythonw.exe", "usb-bridge.exe", "usbgw.exe", exeBaseName()}, name)
	if !known {
		return 0
	}
	fmt.Printf("[bridge] port %d held by our family process pid %d (%s), healthz marker matched\n", port, pid, name)
	return pid
}
