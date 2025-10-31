//go:build !darwin

package main

type mountedVol struct {
	MountPoint string
	Device     string
	FSType     string
	SizeBytes  int64
}

func listMountedDarwin() []mountedVol { return nil }

func findDarwinDeviceForMount(_ string) (string, string) { return "", "" }
