//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// processEntry32 is the TOOLHELP32 snapshot record returned by
// Process32FirstW/Process32NextW.
type processEntry32 struct {
	dwSize              uint32
	cntUsage            uint32
	th32ProcessID       uint32
	th32DefaultHeapID   uintptr
	th32ModuleID        uint32
	cntThreads          uint32
	th32ParentProcessID uint32
	pcPriClassBase      int32
	dwFlags             uint32
	szExeFile           [syscall.MAX_PATH]uint16
}

var (
	modkernel32              = syscall.NewLazyDLL("kernel32.dll")
	procCreateToolhelp32Snap = modkernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW      = modkernel32.NewProc("Process32FirstW")
	procProcess32NextW       = modkernel32.NewProc("Process32NextW")
)

const th32csSnapProcess = 2

// liveProcs returns a point-in-time list of every process. The Git for Windows
// login shell is started as a launcher stub (bin\sh.exe) with the real shell
// (usr\bin\bash.exe) behind it, so callers walk the subtree instead of
// assuming the shell is a direct child.
//
// The snapshot lists process objects, not running ones: a process that has
// exited while someone still holds a handle to it keeps appearing, so
// membership here is not proof of life (see procRunning).
func liveProcs() []procInfo {
	h, _, err := procCreateToolhelp32Snap.Call(th32csSnapProcess, 0)
	if h == ^uintptr(0) { // INVALID_HANDLE_VALUE
		_ = err
		return nil
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var e processEntry32
	e.dwSize = uint32(unsafe.Sizeof(e))
	next := procProcess32FirstW
	out := []procInfo{}
	for {
		r1, _, _ := next.Call(h, uintptr(unsafe.Pointer(&e)))
		if r1 == 0 {
			return out
		}
		next = procProcess32NextW
		out = append(out, procInfo{
			pid:  int(e.th32ProcessID),
			ppid: int(e.th32ParentProcessID),
			name: syscall.UTF16ToString(e.szExeFile[:]),
		})
	}
}

// procRunning reports whether pid is still executing. A process object that
// has already exited stays visible in a snapshot while another handle pins it,
// so its exit code has to be read rather than its presence checked.
func procRunning(pid int) bool {
	return pidAlive(pid)
}
