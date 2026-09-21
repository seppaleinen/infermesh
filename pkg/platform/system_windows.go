//go:build windows

package platform

import (
	"syscall"
	"unsafe"
)

func readDiskFree(path string) (float64, error) {
	var freeBytesAvailable uint64
	var totalBytes uint64
	var totalBytesPerSecond uint64
	var bytesAvailableForLowIOPrio uint64

	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}

	k32 := syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceExW := k32.NewProc("GetDiskFreeSpaceExW")

	r1, _, err := getDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(ptr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalBytesPerSecond)),
		uintptr(unsafe.Pointer(&bytesAvailableForLowIOPrio)),
	)
	if r1 == 0 {
		return 0, err
	}

	return float64(freeBytesAvailable) / (1024 * 1024 * 1024), nil
}
