//go:build darwin

package main

import "fmt"

// On macOS, generic low-level track format is not available via a stable API for USB floppies.
// We return a clear message; users should supply pre-formatted media.
func tryLowLevelFormat(device string, g geom) error {
	return fmt.Errorf("low-level format not supported on macOS for %s; use pre-formatted media", device)
}
