//go:build linux

package main

import (
	"fmt"
	"strings"
)

// Placeholder: proper low-level format requires FDC ioctls (/dev/fdX) or SCSI FORMAT UNIT for USB bridges.
// For now, only allow a friendly message unless this is a classic FDC device.
func tryLowLevelFormat(device string, g geom) error {
	base := device
	if strings.HasPrefix(base, "/dev/fd") {
		return fmt.Errorf("low-level format for %s not implemented yet (FDC ioctls)", device)
	}
	return fmt.Errorf("device %s does not expose a low-level format primitive; use pre-formatted media", device)
}
