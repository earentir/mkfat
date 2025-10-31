//go:build windows

package main

import (
	"io"
	"os"
)

// getDeviceSize on Windows: try regular file seek; for devices, return an error if unsupported
func getDeviceSize(f *os.File) (int64, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err == nil {
		_, _ = f.Seek(0, io.SeekStart)
		return size, nil
	}
	// Windows device size probing is not implemented; require regular files
	return 0, os.ErrInvalid
}


