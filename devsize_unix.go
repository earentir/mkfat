//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"
)

// getDeviceSize returns the size of a file or block device in bytes (Unix variants)
func getDeviceSize(f *os.File) (int64, error) {
	// Try to seek to end (works for regular files)
	size, err := f.Seek(0, io.SeekEnd)
	if err == nil {
		_, _ = f.Seek(0, io.SeekStart)
		return size, nil
	}

	// For block devices on macOS/BSD, use DKIOCGETBLOCKCOUNT + DKIOCGETBLOCKSIZE
	const (
		DKIOCGETBLOCKSIZE  = 0x40046418 // _IOR('d', 24, uint32)
		DKIOCGETBLOCKCOUNT = 0x40086419 // _IOR('d', 25, uint64)
	)

	var blockSize uint32
	var blockCount uint64

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), DKIOCGETBLOCKSIZE, uintptr(unsafe.Pointer(&blockSize)))
	if errno != 0 {
		// Try Linux BLKGETSIZE64
		const BLKGETSIZE64 = 0x80081272
		var sizeBytes uint64
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), BLKGETSIZE64, uintptr(unsafe.Pointer(&sizeBytes)))
		if errno != 0 {
			return 0, fmt.Errorf("cannot determine device size: %v", errno)
		}
		return int64(sizeBytes), nil
	}

	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), DKIOCGETBLOCKCOUNT, uintptr(unsafe.Pointer(&blockCount)))
	if errno != 0 {
		return 0, fmt.Errorf("cannot get block count: %v", errno)
	}

	return int64(blockSize) * int64(blockCount), nil
}


