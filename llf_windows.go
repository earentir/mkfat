//go:build windows

package main

import "fmt"

// Placeholder: low-level format over SPTI (FORMAT UNIT) is device-specific and requires admin.
func tryLowLevelFormat(device string, g geom) error {
	return fmt.Errorf("low-level format not implemented for Windows device %s", device)
}
