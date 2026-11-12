//go:build windows

package app

import (
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// diskFree 返回 path 所在磁盘上当前用户还能用的字节数；拿不到时返回 0。
func diskFree(path string) int64 {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var available uint64
	ok, _, _ := getDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&available)), 0, 0)
	if ok == 0 || available > 1<<62 {
		return 0
	}
	return int64(available)
}
