//go:build darwin

package main

import (
    "path/filepath"
    "golang.org/x/sys/unix"
)

func findDarwinDeviceForMount(target string) (device string, mountpoint string) {
    var buf []unix.Statfs_t
    n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
    if err != nil || n <= 0 {
        return "", ""
    }
    buf = make([]unix.Statfs_t, n)
    _, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
    if err != nil {
        return "", ""
    }
    for _, st := range buf {
        from := bytesToStringDarwin(st.Mntfromname[:])
        on := bytesToStringDarwin(st.Mntonname[:])
        if filepath.Clean(on) == filepath.Clean(target) {
            return from, on
        }
    }
    return "", ""
}

func bytesToStringDarwin(b []byte) string {
    n := 0
    for n < len(b) && b[n] != 0 { n++ }
    runes := make([]rune, n)
    for i := 0; i < n; i++ { runes[i] = rune(b[i]) }
    return string(runes)
}

type mountedVol struct {
    MountPoint string
    Device     string
    FSType     string
    SizeBytes  int64
}

func listMountedDarwin() []mountedVol {
    var out []mountedVol
    n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
    if err != nil || n <= 0 {
        return out
    }
    buf := make([]unix.Statfs_t, n)
    _, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
    if err != nil { return out }
    for _, st := range buf {
        mnt := bytesToStringDarwin(st.Mntonname[:])
        from := bytesToStringDarwin(st.Mntfromname[:])
        fstype := bytesToStringDarwin(st.Fstypename[:])
        size := int64(st.Blocks) * int64(st.Bsize)
        out = append(out, mountedVol{MountPoint: filepath.Clean(mnt), Device: from, FSType: fstype, SizeBytes: size})
    }
    return out
}
