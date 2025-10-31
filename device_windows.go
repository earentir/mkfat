//go:build windows

package main

import (
    "fmt"
    "os"
    "strings"
    "unsafe"
    "golang.org/x/sys/windows"
)

const (
    FSCTL_LOCK_VOLUME     = 0x90018
    FSCTL_DISMOUNT_VOLUME = 0x90020
    FSCTL_UNLOCK_VOLUME   = 0x9001c
    FILE_FLAG_WRITE_THROUGH = 0x80000000
)

const IOCTL_STORAGE_GET_DEVICE_NUMBER = 0x2D1080

type storageDeviceNumber struct {
    DeviceType      uint32
    DeviceNumber    uint32
    PartitionNumber uint32
}

// normalizeWindowsDevicePath maps \\.\A: to \\.\PhysicalDriveN if possible.
// If mapping fails or not a drive-letter path, returns the input unchanged.
func normalizeWindowsDevicePath(p string) string {
    if len(p) < 6 || !strings.HasPrefix(p, `\\.\`) {
        return p
    }
    letter := p[4:5]
    if letter < "A" || letter > "Z" {
        return p
    }
    vol := `\\.\` + letter + `:`
    h, err := windows.CreateFile(
        windows.StringToUTF16Ptr(vol),
        windows.GENERIC_READ,
        windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
        nil,
        windows.OPEN_EXISTING,
        0,
        0,
    )
    if err != nil {
        return p
    }
    defer windows.CloseHandle(h)

    k32 := windows.NewLazySystemDLL("kernel32.dll")
    deviceIoControl := k32.NewProc("DeviceIoControl")
    var out storageDeviceNumber
    var bytesReturned uint32
    r1, _, _ := deviceIoControl.Call(
        uintptr(h),
        IOCTL_STORAGE_GET_DEVICE_NUMBER,
        0, 0,
        uintptr(unsafe.Pointer(&out)), uintptr(unsafe.Sizeof(out)),
        uintptr(unsafe.Pointer(&bytesReturned)),
        0,
    )
    if r1 == 0 {
        return p
    }
    return fmt.Sprintf(`\\.\\PhysicalDrive%d`, out.DeviceNumber)
}

func driveTypeString(t uint32) string {
	switch t {
	case 2:
		return "removable"
	case 3:
		return "fixed"
	case 4:
		return "network"
	case 5:
		return "cdrom"
	case 6:
		return "ramdisk"
	default:
		return "unknown"
	}

}

func getDriveType(root string) uint32 {
    k32 := windows.NewLazySystemDLL("kernel32.dll")
    proc := k32.NewProc("GetDriveTypeW")
    p, _ := windows.UTF16PtrFromString(root)
    r0, _, _ := proc.Call(uintptr(unsafe.Pointer(p)))
    return uint32(r0)
}

func getTotalBytes(root string) uint64 {
    k32 := windows.NewLazySystemDLL("kernel32.dll")
    proc := k32.NewProc("GetDiskFreeSpaceExW")
    p, _ := windows.UTF16PtrFromString(root)
    var total uint64
    _, _, _ = proc.Call(
        uintptr(unsafe.Pointer(p)),
        0,
        uintptr(unsafe.Pointer(&total)),
        0,
    )
    return total
}

// prepareWindowsDevice attempts to lock and dismount a volume before raw access.
// Returns the volume handle if locked, which must be kept open during formatting.
func prepareWindowsDevice(devicePath string) (windows.Handle, error) {
    // Only attempt for drive letter paths like \\.\A:
    if len(devicePath) < 6 || !strings.HasPrefix(devicePath, `\\.\`) {
        return 0, nil // Skip for PhysicalDrive paths
    }
    driveLetter := devicePath[4:5]
    if driveLetter < "A" || driveLetter > "Z" {
        return 0, nil // Not a drive letter
    }
    
    volumePath := `\\.\` + driveLetter + `:`
    
    // Open volume handle with exclusive access
    volHandle, err := windows.CreateFile(
        windows.StringToUTF16Ptr(volumePath),
        windows.GENERIC_READ|windows.GENERIC_WRITE,
        0, // No sharing - exclusive access
        nil,
        windows.OPEN_EXISTING,
        0,
        0,
    )
    if err != nil {
        return 0, fmt.Errorf("cannot open volume %s (may need admin privileges): %w", volumePath, err)
    }
    
    k32 := windows.NewLazySystemDLL("kernel32.dll")
    deviceIoControl := k32.NewProc("DeviceIoControl")
    
    // Lock volume
    var bytesReturned uint32
    r1, _, lastErr := deviceIoControl.Call(
        uintptr(volHandle),
        FSCTL_LOCK_VOLUME,
        0,
        0,
        0,
        0,
        uintptr(unsafe.Pointer(&bytesReturned)),
        0,
    )
    if r1 == 0 {
        windows.CloseHandle(volHandle)
        if lastErr == windows.ERROR_NOT_SUPPORTED {
            return 0, nil // Lock not supported, continue anyway
        }
        return 0, fmt.Errorf("cannot lock volume %s (volume may be in use - close all programs accessing it): %w", volumePath, lastErr)
    }
    
    // Dismount volume (unmounts the filesystem so we can access raw device)
    r1, _, lastErr = deviceIoControl.Call(
        uintptr(volHandle),
        FSCTL_DISMOUNT_VOLUME,
        0,
        0,
        0,
        0,
        uintptr(unsafe.Pointer(&bytesReturned)),
        0,
    )
    if r1 == 0 {
        // If dismount fails, unlock and close
        deviceIoControl.Call(
            uintptr(volHandle),
            FSCTL_UNLOCK_VOLUME,
            0,
            0,
            0,
            0,
            uintptr(unsafe.Pointer(&bytesReturned)),
            0,
        )
        windows.CloseHandle(volHandle)
        if lastErr != windows.ERROR_NOT_SUPPORTED && lastErr != windows.ERROR_NOT_LOCKED {
            return 0, fmt.Errorf("cannot dismount volume %s: %w", volumePath, lastErr)
        }
        // If not supported, that's okay, continue
        return 0, nil
    }
    
    // Success: return handle to keep volume locked during formatting
    return volHandle, nil
}

// openWindowsDevice opens a Windows device with proper flags for raw access
func openWindowsDevice(devicePath string) (*os.File, error) {
    // For drive letters, we need exclusive access after dismounting
    // For PhysicalDrive paths, also use exclusive access
    shareMode := uint32(0) // Exclusive access for raw disk writes
    
    // Open device with WRITE_THROUGH so writes go directly to disk
    handle, err := windows.CreateFile(
        windows.StringToUTF16Ptr(devicePath),
        windows.GENERIC_READ|windows.GENERIC_WRITE,
        shareMode,
        nil,
        windows.OPEN_EXISTING,
        FILE_FLAG_WRITE_THROUGH,
        0,
    )
    if err != nil {
        return nil, fmt.Errorf("cannot open device %s: %w (ensure you are running as administrator and no programs have the drive open)", devicePath, err)
    }
    
    // Convert Windows handle to *os.File
    file := os.NewFile(uintptr(handle), devicePath)
    if file == nil {
        windows.CloseHandle(handle)
        return nil, fmt.Errorf("cannot create file from handle")
    }
    
    return file, nil
}

// cleanupWindowsVolume unlocks and closes a volume handle
func cleanupWindowsVolume(volHandle interface{}) {
    h, ok := volHandle.(windows.Handle)
    if !ok || h == 0 {
        return
    }
    k32 := windows.NewLazySystemDLL("kernel32.dll")
    deviceIoControl := k32.NewProc("DeviceIoControl")
    var bytesReturned uint32
    deviceIoControl.Call(
        uintptr(h),
        FSCTL_UNLOCK_VOLUME,
        0, 0, 0, 0,
        uintptr(unsafe.Pointer(&bytesReturned)),
        0,
    )
    windows.CloseHandle(h)
}

func listMountedWindows() []mountedVol {
	out := []mountedVol{}
	for l := byte('A'); l <= byte('Z'); l++ {
		root := fmt.Sprintf("%c:\\", l)
        typeCode := getDriveType(root)
		if typeCode == 0 || typeCode == 1 { // unknown or no root dir
			continue
		}
        totalNumberOfBytes := getTotalBytes(root)
		out = append(out, mountedVol{
			MountPoint: root,
			Device:     fmt.Sprintf("%c:", l),
			FSType:     driveTypeString(typeCode),
			SizeBytes:  int64(totalNumberOfBytes),
		})
	}
	return out
}


