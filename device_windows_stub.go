//go:build !windows

package main

import (
    "fmt"
    "os"
)

func listMountedWindows() []mountedVol { return nil }

func prepareWindowsDevice(_ string) (interface{}, error) { return nil, nil }

func cleanupWindowsVolume(_ interface{}) {}

func openWindowsDevice(devicePath string) (*os.File, error) {
    // Stub: should never be called on non-Windows
    return nil, fmt.Errorf("openWindowsDevice called on non-Windows platform")
}

func normalizeWindowsDevicePath(p string) string { return p }


