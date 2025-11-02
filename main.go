// mkfat.go
// FAT12/16/32 formatter for images or block devices.
// Cobra CLI + tcell fullscreen UI styled like old DOS formatter.
// One glyph per SECTOR. No percentages.
//
// Build:
//
//	go get github.com/spf13/cobra@v1
//	go get github.com/gdamore/tcell/v2
//	go build -o mkfat .
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"mkfat/retrodfrg"
)

/* ===================== FAT types and geometry ===================== */

// FATType represents the type of FAT filesystem (12, 16, or 32-bit)
type FATType int

// FAT filesystem types
const (
	FAT12 FATType = 12
	FAT16 FATType = 16
	FAT32 FATType = 32
)

type geom struct {
	BytesPerSector    uint16
	SectorsPerCluster uint8
	ReservedSectors   uint16
	NumFATs           uint8
	RootEntries       uint16
	TotalSectors16    uint16
	Media             uint8
	SectorsPerFAT16   uint16
	SectorsPerTrack   uint16
	NumHeads          uint16
	HiddenSectors     uint32
	TotalSectors32    uint32
	SectorsPerFAT32   uint32
	RootCluster       uint32
	FSInfoSector      uint16
	BackupBootSector  uint16
}

func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
}

func parseSize(s string) (int64, error) {
	ss := strings.TrimSpace(strings.ToLower(s))
	if ss == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(ss, "k"):
		mult = 1024
		ss = strings.TrimSuffix(ss, "k")
	case strings.HasSuffix(ss, "m"):
		mult = 1024 * 1024
		ss = strings.TrimSuffix(ss, "m")
	case strings.HasSuffix(ss, "g"):
		mult = 1024 * 1024 * 1024
		ss = strings.TrimSuffix(ss, "g")
	case strings.HasSuffix(ss, "b"):
		mult = 1
		ss = strings.TrimSuffix(ss, "b")
	}
	v, err := strconv.ParseFloat(ss, 64)
	if err != nil {
		return 0, err
	}
	return int64(v * float64(mult)), nil
}

func padRight(s string, n int) []byte {
	if len(s) > n {
		s = s[:n]
	}
	b := make([]byte, n)
	copy(b, s)
	for i := len(s); i < n; i++ {
		b[i] = ' '
	}
	return b
}

func human(b int64) string {
	if b >= 1024*1024 {
		return fmt.Sprintf("%dM", b/(1024*1024))
	}
	if b >= 1024 {
		return fmt.Sprintf("%dK", b/1024)
	}
	return fmt.Sprintf("%dB", b)
}

// getDeviceSize is implemented per-OS in devsize_*.go

func presetForSizeBytes(ft FATType, size int64) (geom, error) {
	g := geom{BytesPerSector: 512, ReservedSectors: 1, NumFATs: 2, Media: 0xF0, NumHeads: 2, HiddenSectors: 0}
	switch size {
	case 360 * 1024:
		g.SectorsPerTrack = 9
		g.RootEntries = 64
		g.SectorsPerCluster = 2
		g.TotalSectors16 = uint16(size / 512)
		g.SectorsPerFAT16 = 2
		return g, nil
	case 720 * 1024:
		g.SectorsPerTrack = 9
		g.RootEntries = 112
		g.SectorsPerCluster = 2
		g.TotalSectors16 = uint16(size / 512)
		g.SectorsPerFAT16 = 3
		return g, nil
	case 1200 * 1024:
		g.SectorsPerTrack = 15
		g.RootEntries = 224
		g.SectorsPerCluster = 1
		g.TotalSectors16 = uint16(size / 512)
		g.SectorsPerFAT16 = 7
		return g, nil
	case 1440 * 1024:
		g.SectorsPerTrack = 18
		g.RootEntries = 224
		g.SectorsPerCluster = 1
		g.TotalSectors16 = uint16(size / 512)
		g.SectorsPerFAT16 = 9
		return g, nil
	case 2880 * 1024:
		g.SectorsPerTrack = 36
		g.RootEntries = 240
		g.TotalSectors16 = uint16(size / 512)
		switch ft {
		case FAT12:
			g.SectorsPerCluster = 1
			g.SectorsPerFAT16 = 9
		case FAT16:
			g.SectorsPerCluster = 2
			g.SectorsPerFAT16 = 18
		}
		return g, nil
	}
	if ft == FAT12 && size < 16*1024*1024 {
		g.SectorsPerTrack = 32
		g.RootEntries = 512
		g.SectorsPerCluster = 1
		ts := uint32(size / 512)
		if ts <= 0xFFFF {
			g.TotalSectors16 = uint16(ts)
		} else {
			g.TotalSectors32 = ts
		}
		g.SectorsPerFAT16 = 16
		return g, nil
	}
	if ft == FAT16 && size <= 32*1024*1024 {
		g.SectorsPerTrack = 32
		g.RootEntries = 512
		switch {
		case size <= 4*1024*1024:
			g.SectorsPerCluster = 2
		case size <= 8*1024*1024:
			g.SectorsPerCluster = 4
		case size <= 16*1024*1024:
			g.SectorsPerCluster = 8
		default:
			g.SectorsPerCluster = 16
		}
		ts := uint32(size / 512)
		if ts <= 0xFFFF {
			g.TotalSectors16 = uint16(ts)
		} else {
			g.TotalSectors32 = ts
		}
		g.SectorsPerFAT16 = 32
		return g, nil
	}
	if ft == FAT32 {
		ts := uint32(size / 512)
		g.Media = 0xF8
		g.RootEntries = 0
		g.ReservedSectors = 32
		g.FSInfoSector = 1
		g.BackupBootSector = 6
		g.RootCluster = 2
		g.SectorsPerTrack = 63
		g.NumHeads = 255
		switch {
		case size <= 260*1024*1024:
			g.SectorsPerCluster = 8
		case size <= 8*1024*1024*1024:
			g.SectorsPerCluster = 8
		case size <= 32*1024*1024*1024:
			g.SectorsPerCluster = 16
		default:
			g.SectorsPerCluster = 32
		}
		if ts <= 0xFFFF {
			g.TotalSectors16 = uint16(ts)
		} else {
			g.TotalSectors32 = ts
		}
		return g, nil
	}
	return g, fmt.Errorf("unsupported size %d for FAT%d", size, ft)
}

func computeLayout(ft FATType, g *geom) (fatSectors, rootDirSectors, dataSectors, clusters uint32, err error) {
	rootDirSectors = ((uint32(g.RootEntries) * 32) + uint32(g.BytesPerSector-1)) / uint32(g.BytesPerSector)
	var totalSectors uint32
	if g.TotalSectors16 != 0 {
		totalSectors = uint32(g.TotalSectors16)
	} else {
		totalSectors = g.TotalSectors32
	}
	if ft == FAT32 {
		rootDirSectors = 0
		if g.ReservedSectors < 32 {
			return 0, 0, 0, 0, errors.New("FAT32 requires >= 32 reserved sectors")
		}
		for i := 0; i < 8; i++ {
			fatSectors = g.SectorsPerFAT32
			if fatSectors == 0 {
				fatSectors = 1
			}
			dataSectors = totalSectors - uint32(g.ReservedSectors) - uint32(g.NumFATs)*fatSectors
			if dataSectors <= 0 {
				return 0, 0, 0, 0, errors.New("dataSectors<=0")
			}
			clusters = dataSectors / uint32(g.SectorsPerCluster)
			entries := clusters + 2
			neededBytes := entries * 4
			need := (neededBytes + uint32(g.BytesPerSector) - 1) / uint32(g.BytesPerSector)
			if need == fatSectors {
				break
			}
			g.SectorsPerFAT32 = need
		}
		if clusters < 65525 {
			return 0, 0, 0, 0, fmt.Errorf("clusters=%d too small for FAT32", clusters)
		}
		return g.SectorsPerFAT32, 0, dataSectors, clusters, nil
	}
	for i := 0; i < 8; i++ {
		fatSectors = uint32(g.SectorsPerFAT16)
		dataSectors = totalSectors - uint32(g.ReservedSectors) - uint32(g.NumFATs)*fatSectors - rootDirSectors
		if dataSectors <= 0 {
			return 0, 0, 0, 0, errors.New("dataSectors<=0")
		}
		clusters = dataSectors / uint32(g.SectorsPerCluster)
		var need uint32
		if ft == FAT12 {
			entries := clusters + 2
			neededBytes := ((entries * 3) + 1) / 2
			need = (neededBytes + uint32(g.BytesPerSector) - 1) / uint32(g.BytesPerSector)
		} else {
			entries := clusters + 2
			neededBytes := entries * 2
			need = (neededBytes + uint32(g.BytesPerSector) - 1) / uint32(g.BytesPerSector)
		}
		if need == fatSectors {
			break
		}
		g.SectorsPerFAT16 = uint16(need)
	}
	if ft == FAT12 && clusters >= 4085 {
		return 0, 0, 0, 0, fmt.Errorf("clusters=%d invalid for FAT12", clusters)
	}
	if ft == FAT16 && (clusters < 4085 || clusters > 65524) {
		return 0, 0, 0, 0, fmt.Errorf("clusters=%d invalid for FAT16", clusters)
	}
	return fatSectors, rootDirSectors, dataSectors, clusters, nil
}

/* ===================== Boot/FAT builders ===================== */

func buildBootSector1216(ft FATType, g geom, volLabel, oem string) []byte {
	if volLabel == "" {
		volLabel = "NO NAME    "
	}
	if oem == "" {
		oem = "EARMKFAT"
	}
	sec := make([]byte, 512)
	sec[0], sec[1], sec[2] = 0xEB, 0x3C, 0x90
	copy(sec[3:11], padRight(oem, 8))
	binary.LittleEndian.PutUint16(sec[11:], g.BytesPerSector)
	sec[13] = g.SectorsPerCluster
	binary.LittleEndian.PutUint16(sec[14:], g.ReservedSectors)
	sec[16] = g.NumFATs
	binary.LittleEndian.PutUint16(sec[17:], g.RootEntries)
	binary.LittleEndian.PutUint16(sec[19:], g.TotalSectors16)
	sec[21] = g.Media
	binary.LittleEndian.PutUint16(sec[22:], g.SectorsPerFAT16)
	binary.LittleEndian.PutUint16(sec[24:], g.SectorsPerTrack)
	binary.LittleEndian.PutUint16(sec[26:], g.NumHeads)
	binary.LittleEndian.PutUint32(sec[28:], g.HiddenSectors)
	binary.LittleEndian.PutUint32(sec[32:], g.TotalSectors32)
	sec[36], sec[37], sec[38] = 0x00, 0x00, 0x29
	binary.LittleEndian.PutUint32(sec[39:], 0x12345678)
	copy(sec[43:54], padRight(volLabel, 11))
	if ft == FAT12 {
		copy(sec[54:62], []byte("FAT12   "))
	} else {
		copy(sec[54:62], []byte("FAT16   "))
	}

	// Add boot code stub (MS-DOS compatible)
	bootCode := []byte{
		// Offset 62 (0x3E): Boot code starts here
		0x0E,             // push cs
		0x1F,             // pop ds
		0xBE, 0x77, 0x7C, // mov si, 0x7C77 (message offset)
		0xAC,       // lodsb
		0x22, 0xC0, // and al, al
		0x74, 0x0B, // jz short 0x0B (halt)
		0x56,       // push si
		0xB4, 0x0E, // mov ah, 0x0E (teletype output)
		0xBB, 0x07, 0x00, // mov bx, 0x0007
		0xCD, 0x10, // int 0x10
		0x5E,       // pop si
		0xEB, 0xF0, // jmp short -16 (loop)
		0x32, 0xE4, // xor ah, ah
		0xCD, 0x16, // int 0x16 (wait for key)
		0xCD, 0x19, // int 0x19 (reboot)
		0xEB, 0xFE, // jmp short -2 (hang)
	}
	copy(sec[62:], bootCode)

	// Boot message at offset 119 (0x77)
	msg := "Non-system disk or disk error\r\nReplace and press any key when ready\r\n\x00"
	copy(sec[119:], []byte(msg))

	sec[510], sec[511] = 0x55, 0xAA
	return sec
}

func buildBootSector32(g geom, volLabel, oem string) []byte {
	if volLabel == "" {
		volLabel = "NO NAME    "
	}
	if oem == "" {
		oem = "EARMKFAT"
	}
	sec := make([]byte, 512)
	sec[0], sec[1], sec[2] = 0xEB, 0x58, 0x90
	copy(sec[3:11], padRight(oem, 8))
	binary.LittleEndian.PutUint16(sec[11:], g.BytesPerSector)
	sec[13] = g.SectorsPerCluster
	binary.LittleEndian.PutUint16(sec[14:], g.ReservedSectors)
	sec[16] = g.NumFATs
	binary.LittleEndian.PutUint16(sec[17:], 0)
	if g.TotalSectors16 != 0 {
		binary.LittleEndian.PutUint16(sec[19:], g.TotalSectors16)
	}
	sec[21] = g.Media
	binary.LittleEndian.PutUint16(sec[22:], 0)
	binary.LittleEndian.PutUint16(sec[24:], g.SectorsPerTrack)
	binary.LittleEndian.PutUint16(sec[26:], g.NumHeads)
	binary.LittleEndian.PutUint32(sec[28:], g.HiddenSectors)
	binary.LittleEndian.PutUint32(sec[32:], g.TotalSectors32)
	binary.LittleEndian.PutUint32(sec[36:], g.SectorsPerFAT32)
	binary.LittleEndian.PutUint16(sec[40:], 0)
	binary.LittleEndian.PutUint16(sec[42:], 0)
	binary.LittleEndian.PutUint32(sec[44:], g.RootCluster)
	binary.LittleEndian.PutUint16(sec[48:], g.FSInfoSector)
	binary.LittleEndian.PutUint16(sec[50:], g.BackupBootSector)
	sec[64], sec[65], sec[66] = 0x80, 0x00, 0x29
	binary.LittleEndian.PutUint32(sec[67:], 0x12345678)
	copy(sec[71:82], padRight(volLabel, 11))
	copy(sec[82:90], []byte("FAT32   "))

	// Add boot code stub (MS-DOS compatible)
	bootCode := []byte{
		// Offset 90 (0x5A): Boot code starts here
		0x0E,             // push cs
		0x1F,             // pop ds
		0xBE, 0xA3, 0x7C, // mov si, 0x7CA3 (message offset)
		0xAC,       // lodsb
		0x22, 0xC0, // and al, al
		0x74, 0x0B, // jz short 0x0B (halt)
		0x56,       // push si
		0xB4, 0x0E, // mov ah, 0x0E (teletype output)
		0xBB, 0x07, 0x00, // mov bx, 0x0007
		0xCD, 0x10, // int 0x10
		0x5E,       // pop si
		0xEB, 0xF0, // jmp short -16 (loop)
		0x32, 0xE4, // xor ah, ah
		0xCD, 0x16, // int 0x16 (wait for key)
		0xCD, 0x19, // int 0x19 (reboot)
		0xEB, 0xFE, // jmp short -2 (hang)
	}
	copy(sec[90:], bootCode)

	// Boot message at offset 163 (0xA3)
	msg := "Non-system disk or disk error\r\nReplace and press any key when ready\r\n\x00"
	copy(sec[163:], []byte(msg))

	sec[510], sec[511] = 0x55, 0xAA
	return sec
}

func buildFSInfo() []byte {
	fs := make([]byte, 512)
	binary.LittleEndian.PutUint32(fs[0:], 0x41615252)
	binary.LittleEndian.PutUint32(fs[484:], 0x61417272)
	binary.LittleEndian.PutUint32(fs[488:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(fs[492:], 0x00000002)
	binary.LittleEndian.PutUint32(fs[508:], 0xAA550000)
	return fs
}

func buildRootLabelEntry(label string) []byte {
	if label == "" {
		return nil
	}
	e := make([]byte, 32)
	copy(e[0:11], padRight(label, 11))
	e[11] = 0x08
	return e
}

func initFAT1216(ft FATType, b []byte, media byte) {
	if ft == FAT12 {
		if len(b) >= 3 {
			b[0] = media
			b[1] = 0xFF
			b[2] = 0xFF
		}
	} else {
		if len(b) >= 4 {
			b[0] = media
			b[1] = 0xFF
			b[2] = 0xFF
			b[3] = 0xFF
		}
	}
}

func initFAT32(b []byte, media byte) {
	put := func(i int, v uint32) {
		o := i * 4
		if o+4 <= len(b) {
			binary.LittleEndian.PutUint32(b[o:], v)
		}
	}
	put(0, 0x0FFFFF00|uint32(media))
	put(1, 0x0FFFFFFF)
	put(2, 0x0FFFFFFF)
}

/* ===================== TUI ===================== */
/* TUI moved to package retrodfrg */

// progressTracker tracks write progress in main.go (not in UI package)
type progressTracker struct {
	progressMap  []bool
	totalSectors int64
	currentPos   int64
}

func newProgressTracker(total int64) *progressTracker {
	return &progressTracker{
		progressMap:  make([]bool, total),
		totalSectors: total,
	}
}

func (pt *progressTracker) markRange(start int64, count int64) {
	end := start + count
	if end > pt.totalSectors {
		end = pt.totalSectors
	}
	for i := start; i < end; i++ {
		if i >= 0 && i < int64(len(pt.progressMap)) {
			pt.progressMap[i] = true
		}
	}
	if end-1 >= 0 {
		pt.currentPos = end - 1
	}
}

func (pt *progressTracker) writtenCount() int64 {
	count := int64(0)
	for _, written := range pt.progressMap {
		if written {
			count++
		}
	}
	return count
}

// updateProgressMapVisualization generates visual progress map from tracker and updates UI.
func updateProgressMapVisualization(ui *retrodfrg.UI, pt *progressTracker, systemRanges [][2]int64, w, h int) {
	if pt.totalSectors <= 0 {
		return
	}

	// Calculate available space
	availRows := h - 7 // leave room for other UI elements
	if availRows < 1 {
		availRows = 1
	}
	totalCells := int64(w * availRows)

	// Calculate start position (scroll to follow current position)
	start := int64(0)
	if pt.totalSectors > totalCells {
		if pt.currentPos >= totalCells-1 {
			start = pt.currentPos - (totalCells - 1)
		}
		if start+totalCells > pt.totalSectors {
			start = pt.totalSectors - totalCells
		}
		if start < 0 {
			start = 0
		}
	}

	// Build visual map
	lines := make([]string, availRows)
	full := '█'
	free := '░'
	sys := '■'

	inSystem := func(abs int64) bool {
		for _, r := range systemRanges {
			if abs >= r[0] && abs <= r[1] {
				return true
			}
		}
		return false
	}

	for row := 0; row < availRows; row++ {
		var b strings.Builder
		b.Grow(w) // Pre-allocate for efficiency
		for col := 0; col < w; col++ {
			idx := int64(row*w + col)
			if idx >= totalCells {
				break
			}
			abs := start + idx
			if abs >= pt.totalSectors {
				break
			}

			var rch rune = free
			if abs >= 0 && abs < int64(len(pt.progressMap)) && pt.progressMap[abs] {
				rch = full
			} else if inSystem(abs) {
				rch = sys
			}
			b.WriteRune(rch)
		}
		lines[row] = b.String()
	}

	ui.SetProgressMap(lines)
}

// updateStatusLines updates the UI status lines with current progress information.
func updateStatusLines(ui *retrodfrg.UI, pt *progressTracker, startTime time.Time, currentOp string, emuRate float64, isEmulate bool, systemRanges [][2]int64) {
	written := pt.writtenCount()
	curPos := pt.currentPos
	totalSectors := pt.totalSectors
	elapsed := time.Since(startTime).Truncate(time.Second)

	// Update progress map visualization
	if ui != nil {
		w, h := ui.Size()
		if w > 0 && h > 0 {
			updateProgressMapVisualization(ui, pt, systemRanges, w, h)
		}
	}

	var rate float64
	if isEmulate {
		rate = emuRate
	} else {
		if elapsed.Seconds() > 0 {
			rate = float64(written*512) / elapsed.Seconds()
		}
	}

	var etaStr string
	if rate > 0 {
		remainBytes := (totalSectors - written) * 512
		eta := time.Duration(float64(remainBytes) / rate * float64(time.Second)).Truncate(time.Second)
		etaStr = eta.String()
	} else {
		etaStr = "—"
	}

	mode := "REAL"
	if isEmulate {
		mode = "EMULATE"
	}

	rateStr := human(int64(rate))

	lines := []string{
		fmt.Sprintf("Absolute: %06d", curPos),
		fmt.Sprintf("Written: %d / %d sectors", written, totalSectors),
		fmt.Sprintf("Elapsed: %s   Rate: %s/s   ETA: %s   Mode: %s", elapsed, rateStr, etaStr, mode),
		"Current op: " + currentOp,
	}
	ui.SetStatusLines(lines)
}

// writeSpanWithStatus writes a buffer and updates status lines periodically.
func writeSpanWithStatus(w io.WriterAt, absStart int64, buf []byte, ui *retrodfrg.UI, pt *progressTracker, currentOp string, startTime time.Time, emuRate float64, isEmulate bool, systemRanges [][2]int64) error {
	const chunk = 1 << 20
	wr := int64(0)
	updateCount := 0
	for wr < int64(len(buf)) {
		n := int64(len(buf)) - wr
		if n > chunk {
			n = chunk
		}
		if _, err := w.WriteAt(buf[wr:wr+n], (absStart*512)+wr); err != nil {
			return err
		}
		secs := n / 512
		if secs <= 0 {
			secs = 1
		}
		pt.markRange(absStart+wr/512, secs)
		if ui.IsStopped() {
			return retrodfrg.ErrInterrupted
		}
		// Update status periodically or on first/last chunk
		if currentOp != "" && (wr == 0 || updateCount%5 == 0 || wr+n >= int64(len(buf))) {
			updateStatusLines(ui, pt, startTime, currentOp, emuRate, isEmulate, systemRanges)
		}
		ui.LayoutAndDraw()
		wr += n
		updateCount++
	}
	if currentOp != "" {
		updateStatusLines(ui, pt, startTime, currentOp, emuRate, isEmulate, systemRanges)
	}
	ui.LayoutAndDraw()
	return nil
}

// zeroSpanWithStatus writes zeroes and updates status lines periodically.
func zeroSpanWithStatus(w io.WriterAt, absStart, sectors int64, ui *retrodfrg.UI, pt *progressTracker, currentOp string, startTime time.Time, emuRate float64, isEmulate bool, systemRanges [][2]int64) error {
	const zSize = 1 << 20
	z := make([]byte, zSize)
	written := int64(0)
	bytes := sectors * 512
	updateCount := 0
	for written < bytes {
		k := bytes - written
		if k > zSize {
			k = zSize
		}
		if _, err := w.WriteAt(z[:k], (absStart*512 + written)); err != nil {
			return err
		}
		secs := k / 512
		if secs <= 0 {
			secs = 1
		}
		pt.markRange(absStart+written/512, secs)
		if ui.IsStopped() {
			return retrodfrg.ErrInterrupted
		}
		// Update status periodically or on first/last chunk
		if currentOp != "" && (written == 0 || updateCount%5 == 0 || written+k >= bytes) {
			updateStatusLines(ui, pt, startTime, currentOp, emuRate, isEmulate, systemRanges)
		}
		ui.LayoutAndDraw()
		written += k
		updateCount++
	}
	if currentOp != "" {
		updateStatusLines(ui, pt, startTime, currentOp, emuRate, isEmulate, systemRanges)
	}
	ui.LayoutAndDraw()
	return nil
}

// waitWithStop waits briefly while allowing early interruption.
func waitWithStop(ui *retrodfrg.UI) error {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	}
}

/* ===================== Emulation pacing ===================== */

func defaultEmuBPS(size int64) float64 {
	// Realistic floppy speeds based on actual hardware
	switch size {
	case 360 * 1024, 720 * 1024:
		return 31.25 * 1024 // DD: 250 kbit/s → ~12s for 360K, ~23s for 720K
	case 1200 * 1024, 1440 * 1024:
		return 62.5 * 1024 // HD: 500 kbit/s → ~19s for 1.2M, ~23s for 1.44M
	case 2880 * 1024:
		return 125 * 1024 // ED: 1 Mbit/s → ~23s for 2.88M
	default:
		return 62.5 * 1024 // default: HD speed
	}
}

// nullWriter implements writerAt but doesn't actually write anything
type nullWriter struct{}

func (n nullWriter) WriteAt(p []byte, _ int64) (int, error) {
	return len(p), nil
}

// old emulateSequence removed; handled inline using retrodfrg helpers

// old waitWithStop removed; use waitWithStop

/* ===================== IO helpers (moved to retrodfrg) ===================== */

// checkBadSector writes and reads back a sector to verify it's good
func checkBadSector(rw interface {
	WriteAt([]byte, int64) (int, error)
	ReadAt([]byte, int64) (int, error)
}, sector int64) error {
	pattern := make([]byte, 512)
	// Write a recognizable pattern
	for i := range pattern {
		pattern[i] = byte(sector & 0xFF)
	}

	offset := sector * 512
	if _, err := rw.WriteAt(pattern, offset); err != nil {
		return fmt.Errorf("bad sector %d (write failed): %w", sector, err)
	}

	// Read it back
	verify := make([]byte, 512)
	if _, err := rw.ReadAt(verify, offset); err != nil {
		return fmt.Errorf("bad sector %d (read failed): %w", sector, err)
	}

	// Verify pattern
	for i := range pattern {
		if pattern[i] != verify[i] {
			return fmt.Errorf("bad sector %d (verification failed)", sector)
		}
	}

	return nil
}

// fullFormatDataArea zeros all data sectors with bad sector detection
func fullFormatDataArea(rw interface {
	WriteAt([]byte, int64) (int, error)
	ReadAt([]byte, int64) (int, error)
}, absStart, sectors int64, u *retrodfrg.UI, pt *progressTracker, currentOp string, startTime time.Time, systemRanges [][2]int64) error {
	const zSize = 1 << 20
	z := make([]byte, zSize)
	written := int64(0)
	bytes := sectors * 512
	badSectors := []int64{}

	for written < bytes {
		k := bytes - written
		if k > zSize {
			k = zSize
		}

		// Write zeros
		if _, err := rw.WriteAt(z[:k], (absStart*512)+written); err != nil {
			return err
		}

		// Update UI and check sectors
		secs := k / 512
		if secs <= 0 {
			secs = 1
		}
		for i := int64(0); i < secs; i++ {
			if u.IsStopped() {
				return retrodfrg.ErrInterrupted
			}

			currentSector := absStart + written/512 + i

			// Check for bad sector (only on real devices, not emulation)
			if true {
				if err := checkBadSector(rw, currentSector); err != nil {
					badSectors = append(badSectors, currentSector)
					// Continue formatting but track bad sectors
				}
			}

			pt.markRange(currentSector, 1)
			// Update status every 10 sectors
			if i%10 == 0 || i == secs-1 {
				updateStatusLines(u, pt, startTime, currentOp, 0, false, systemRanges)
			}
			u.LayoutAndDraw()
		}
		written += k
	}

	if len(badSectors) > 0 {
		return fmt.Errorf("found %d bad sector(s): %v", len(badSectors), badSectors)
	}

	return nil
}

// old eventLoop removed; handled inside retrodfrg.UI

/* ===================== Main ===================== */

func printGeometryInfo(ft FATType, sz int64, g geom, fatSecs, rootSecs, dataSecs, _ uint32, label, oem string) {
	totalSectors := int64(sz / 512)
	cylinders := int(totalSectors) / int(g.SectorsPerTrack) / int(g.NumHeads)

	absStartFAT1 := int64(g.ReservedSectors)
	absStartFAT2 := absStartFAT1 + int64(fatSecs)
	absStartRoot := int64(g.ReservedSectors) + int64(g.NumFATs)*int64(fatSecs)
	absStartData := absStartRoot + int64(rootSecs)
	if ft == FAT32 {
		absStartRoot = -1
		absStartData = int64(g.ReservedSectors) + int64(g.NumFATs)*int64(fatSecs)
	}

	lineWidth := 79
	barHeavy := strings.Repeat("═", lineWidth)
	barLight := strings.Repeat("─", lineWidth)

	labelDisplay := strings.TrimSpace(label)
	if labelDisplay == "" {
		labelDisplay = "NO NAME"
	}
	labelDisplay = strings.ToUpper(labelDisplay)

	oemDisplay := strings.TrimSpace(oem)
	if oemDisplay == "" {
		oemDisplay = "EARMKFAT"
	}
	oemDisplay = strings.ToUpper(oemDisplay)

	plural := "s"
	if g.SectorsPerCluster == 1 {
		plural = ""
	}
	clusterBytes := int(g.SectorsPerCluster) * int(g.BytesPerSector)

	formatRange := func(start, end int64) string {
		if end <= start {
			return fmt.Sprintf("[%06d]", start)
		}
		return fmt.Sprintf("[%06d … %06d]", start, end)
	}

	lines := []string{
		barHeavy,
		" GEOMETRY",
		barLight,
		fmt.Sprintf(" Bytes/Sector: %-4d    Sectors/Track: %-2d    Heads: %-2d   Cylinders: %d", g.BytesPerSector, g.SectorsPerTrack, g.NumHeads, cylinders),
		fmt.Sprintf(" Reserved: %-4d    FATs: %-2d  Root entries: %d", g.ReservedSectors, g.NumFATs, g.RootEntries),
		fmt.Sprintf(" Sectors/FAT: %-5d   RootDir sectors: %-5d   Data sectors: %d", fatSecs, rootSecs, dataSecs),
		fmt.Sprintf(" Cluster size: %d sector%s (%d bytes)  Total sectors: %d", g.SectorsPerCluster, plural, clusterBytes, totalSectors),
		fmt.Sprintf(" OEM: %s  Label: %s", oemDisplay, labelDisplay),
		barLight,
		" LAYOUT (absolute sector ranges)",
		barLight,
		fmt.Sprintf(" Boot  : %s", formatRange(0, 0)),
		fmt.Sprintf(" FAT #1: %s    FAT #2: %s", formatRange(absStartFAT1, absStartFAT1+int64(fatSecs)-1), formatRange(absStartFAT2, absStartFAT2+int64(fatSecs)-1)),
	}

	dataRange := formatRange(absStartData, totalSectors-1)
	if ft != FAT32 {
		lines = append(lines, fmt.Sprintf(" Root  : %s    Data  : %s", formatRange(absStartRoot, absStartRoot+int64(rootSecs)-1), dataRange))
	} else {
		lines = append(lines, fmt.Sprintf(" Data  : %s", dataRange))
	}

	lines = append(lines, barHeavy)

	for _, line := range lines {
		fmt.Printf("\r%s\n", line)
	}
	fmt.Println()
}

/* ===================== Copy operations ===================== */

func copyDeviceToImage(devicePath, imagePath string, blockSize int64) error {
	// Open source device
	src, err := os.OpenFile(devicePath, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open device: %w", err)
	}
	defer src.Close()

	// Get device size
	deviceSize, err := getDeviceSize(src)
	if err != nil {
		return fmt.Errorf("get device size: %w", err)
	}

	// Create destination image
	if err := os.MkdirAll(filepath.Dir(imagePath), 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	dst, err := os.Create(imagePath)
	if err != nil {
		return fmt.Errorf("create image: %w", err)
	}
	defer dst.Close()

	fmt.Printf("Copying %s (%s) to %s...\n", devicePath, human(deviceSize), imagePath)

	// Copy block by block
	buf := make([]byte, blockSize)
	var totalCopied int64

	for totalCopied < deviceSize {
		n, err := src.Read(buf)
		if err != nil && err != io.EOF {
			return fmt.Errorf("read device: %w", err)
		}
		if n == 0 {
			break
		}

		if _, err := dst.Write(buf[:n]); err != nil {
			return fmt.Errorf("write image: %w", err)
		}

		totalCopied += int64(n)

		// Progress indicator
		if totalCopied%(blockSize*1000) == 0 || totalCopied >= deviceSize {
			percent := float64(totalCopied) * 100.0 / float64(deviceSize)
			fmt.Printf("\rProgress: %s / %s (%.1f%%)", human(totalCopied), human(deviceSize), percent)
		}
	}

	fmt.Printf("\nCopy complete: %s copied\n", human(totalCopied))
	return nil
}

func copyImageToDevice(imagePath, devicePath string, blockSize int64) error {
	// Open source image
	src, err := os.Open(imagePath)
	if err != nil {
		return fmt.Errorf("open image: %w", err)
	}
	defer src.Close()

	// Get image size
	imageStat, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat image: %w", err)
	}
	imageSize := imageStat.Size()

	// Open destination device
	dst, err := os.OpenFile(devicePath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open device: %w", err)
	}
	defer dst.Close()

	// Validate device size
	deviceSize, err := getDeviceSize(dst)
	if err != nil {
		return fmt.Errorf("get device size: %w", err)
	}
	if deviceSize < imageSize {
		return fmt.Errorf("device too small: has %s, need %s", human(deviceSize), human(imageSize))
	}

	fmt.Printf("Copying %s (%s) to %s...\n", imagePath, human(imageSize), devicePath)
	if deviceSize > imageSize {
		fmt.Printf("WARNING: device is %s, only writing %s\n", human(deviceSize), human(imageSize))
	}

	// Copy block by block
	buf := make([]byte, blockSize)
	var totalCopied int64

	for totalCopied < imageSize {
		n, err := src.Read(buf)
		if err != nil && err != io.EOF {
			return fmt.Errorf("read image: %w", err)
		}
		if n == 0 {
			break
		}

		if _, err := dst.Write(buf[:n]); err != nil {
			return fmt.Errorf("write device: %w", err)
		}

		totalCopied += int64(n)

		// Progress indicator
		if totalCopied%(blockSize*1000) == 0 || totalCopied >= imageSize {
			percent := float64(totalCopied) * 100.0 / float64(imageSize)
			fmt.Printf("\rProgress: %s / %s (%.1f%%)", human(totalCopied), human(imageSize), percent)
		}
	}

	// Sync to ensure all data is written
	if err := dst.Sync(); err != nil {
		return fmt.Errorf("sync device: %w", err)
	}

	fmt.Printf("\nCopy complete: %s written to device\n", human(totalCopied))
	return nil
}

func main() {
	root := &cobra.Command{
		Use:   "mkfat",
		Short: "FAT filesystem formatter and disk imaging utility",
		Long:  "Create FAT12/16/32 filesystems on images or devices, and copy disk images",
	}

	// Format command
	var (
		ftStr, sizeStr, out, device, label, oem string
		heads, spt, tracks                      int
		force, emulate, fullFormat              bool
		syncMode                                string
		uiEvery                                 int
		verifyTrack                             bool
		attemptLLF                              bool
	)

	formatCmd := &cobra.Command{
		Use:   "format",
		Short: "Format an image or block device as FAT12/16/32",
		RunE: func(_ *cobra.Command, _ []string) error {
			targets := 0
			if out != "" {
				targets++
			}
			if device != "" {
				targets++
			}
			if targets > 1 {
				return fmt.Errorf("choose at most one of --out or --device")
			}
			if !emulate && targets == 0 {
				return fmt.Errorf("choose --out or --device, or use --emulate")
			}
			if device != "" && !force {
				return fmt.Errorf("--device requires --force")
			}
			// Windows: disallow raw device formatting to USB floppies
			if device != "" && runtime.GOOS == "windows" {
				return fmt.Errorf("raw device formatting is not supported on Windows USB floppies; create an image with --out and write it from Linux/macOS or with a specialized tool")
			}
			if sizeStr == "" {
				return fmt.Errorf("--size is required")
			}
			sz, err := parseSize(sizeStr)
			if err != nil {
				return err
			}
			if sz%512 != 0 {
				return fmt.Errorf("size must be multiple of 512")
			}

			var ft FATType
			switch strings.ToLower(ftStr) {
			case "fat12":
				ft = FAT12
			case "fat16":
				ft = FAT16
			case "fat32":
				ft = FAT32
			default:
				return fmt.Errorf("unknown --type %q", ftStr)
			}

			g, err := presetForSizeBytes(ft, sz)
			if err != nil {
				return err
			}
			if heads > 0 {
				g.NumHeads = uint16(heads)
			}
			if spt > 0 {
				g.SectorsPerTrack = uint16(spt)
			}
			if tracks > 0 {
				total := uint32(tracks) * uint32(g.NumHeads) * uint32(g.SectorsPerTrack)
				if total <= 0xFFFF {
					g.TotalSectors16 = uint16(total)
					g.TotalSectors32 = 0
				} else {
					g.TotalSectors16 = 0
					g.TotalSectors32 = total
				}
			} else {
				total := uint32(sz / 512)
				if total <= 0xFFFF {
					g.TotalSectors16 = uint16(total)
					g.TotalSectors32 = 0
				} else {
					g.TotalSectors16 = 0
					g.TotalSectors32 = total
				}
			}
			fatSecs, rootSecs, dataSecs, clusters, err := computeLayout(ft, &g)
			if err != nil {
				return err
			}

			ui, err := retrodfrg.NewUI()
			if err != nil {
				return fmt.Errorf("ui init: %w", err)
			}
			defer ui.Close()

			startTime := time.Now()
			totalSectors := int64(sz / 512)
			pt := newProgressTracker(totalSectors)

			// Generic UI config
			ui.SetTitle(fmt.Sprintf("FORMAT – DRIVE %s:  FAT%d  %d bytes", "A", ft, sz))
			ui.SetPhases([]string{"Boot", "FAT1", "FAT2", "Root"})
			// Compute absolute ranges
			absFAT1 := int64(g.ReservedSectors)
			absFAT2 := absFAT1 + int64(fatSecs)
			absRoot := int64(-1)
			absData := int64(g.ReservedSectors) + int64(g.NumFATs)*int64(fatSecs)
			if ft != FAT32 {
				absRoot = absData
				absData += int64(rootSecs)
			}
			systemRanges := [][2]int64{
				{0, 0},
				{absFAT1, absFAT1 + int64(fatSecs) - 1},
				{absFAT2, absFAT2 + int64(fatSecs) - 1},
			}
			if ft != FAT32 {
				systemRanges = append(systemRanges, [2]int64{absRoot, absRoot + int64(rootSecs) - 1})
			}
			ui.SetSummaryLines([]string{
				fmt.Sprintf("Bytes/Sector: %-4d  Sectors/Track: %-2d  Heads: %-2d", g.BytesPerSector, g.SectorsPerTrack, g.NumHeads),
				fmt.Sprintf("Reserved: %-3d  FATs: %-1d  Root entries: %-3d", g.ReservedSectors, g.NumFATs, g.RootEntries),
				fmt.Sprintf("Sectors/FAT: %-4d  RootDir sectors: %-3d  Data sectors: %-4d", fatSecs, rootSecs, dataSecs),
			})
			ui.SetLegend([]string{
				"Legend:  █ formatted/written   ░ not yet written   ■ system area | Q to quit",
			})

			// Setup Ctrl+C handler to exit immediately
			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sigChan
				ui.RequestStop()
				fmt.Fprintf(os.Stderr, "\nInterrupted\n")
				os.Exit(130)
			}()

			if emulate {
				emuRate := defaultEmuBPS(sz)
				updateStatusLines(ui, pt, startTime, "Write boot sector", emuRate, true, systemRanges)
				ui.LayoutAndDraw()
				// Emulate using nullWriter and helpers
				nw := nullWriter{}
				// boot
				var boot []byte
				if ft == FAT32 {
					boot = buildBootSector32(g, label, oem)
				} else {
					boot = buildBootSector1216(ft, g, label, oem)
				}
				if err := writeSpanWithStatus(nw, 0, boot, ui, pt, "Write boot sector", startTime, emuRate, true, systemRanges); err != nil && !errors.Is(err, retrodfrg.ErrInterrupted) {
					return err
				}
				ui.SetPhaseDone("boot")
				updateStatusLines(ui, pt, startTime, "Write boot sector", emuRate, true, systemRanges)
				ui.LayoutAndDraw()
				if ft == FAT32 {
					updateStatusLines(ui, pt, startTime, "Write FSInfo", emuRate, true, systemRanges)
					ui.LayoutAndDraw()
					fsinfo := buildFSInfo()
					if err := writeSpanWithStatus(nw, int64(g.FSInfoSector), fsinfo, ui, pt, "Write FSInfo", startTime, emuRate, true, systemRanges); err != nil && !errors.Is(err, retrodfrg.ErrInterrupted) {
						return err
					}
					updateStatusLines(ui, pt, startTime, "Backup boot sector", emuRate, true, systemRanges)
					ui.LayoutAndDraw()
					if err := writeSpanWithStatus(nw, int64(g.BackupBootSector), boot, ui, pt, "Backup boot sector", startTime, emuRate, true, systemRanges); err != nil && !errors.Is(err, retrodfrg.ErrInterrupted) {
						return err
					}
				}
				// FATs
				updateStatusLines(ui, pt, startTime, "Initialize FAT #1", emuRate, true, systemRanges)
				ui.LayoutAndDraw()
				fatBytes := int64(fatSecs) * int64(g.BytesPerSector)
				fatBuf := make([]byte, fatBytes)
				if ft == FAT32 {
					initFAT32(fatBuf, g.Media)
				} else {
					initFAT1216(ft, fatBuf, g.Media)
				}
				if err := writeSpanWithStatus(nw, int64(g.ReservedSectors), fatBuf, ui, pt, "Initialize FAT #1", startTime, emuRate, true, systemRanges); err != nil && !errors.Is(err, retrodfrg.ErrInterrupted) {
					return err
				}
				ui.SetPhaseDone("fat1")
				updateStatusLines(ui, pt, startTime, "Initialize FAT #1", emuRate, true, systemRanges)
				ui.LayoutAndDraw()
				if err := writeSpanWithStatus(nw, int64(g.ReservedSectors)+int64(fatSecs), fatBuf, ui, pt, "Initialize FAT #2", startTime, emuRate, true, systemRanges); err != nil && !errors.Is(err, retrodfrg.ErrInterrupted) {
					return err
				}
				ui.SetPhaseDone("fat2")
				updateStatusLines(ui, pt, startTime, "Initialize FAT #2", emuRate, true, systemRanges)
				ui.LayoutAndDraw()
				// Root (if 12/16)
				if ft != FAT32 {
					updateStatusLines(ui, pt, startTime, "Clear root directory", emuRate, true, systemRanges)
					ui.LayoutAndDraw()
					absRoot := int64(g.ReservedSectors) + int64(g.NumFATs)*int64(fatSecs)
					if err := zeroSpanWithStatus(nw, absRoot, int64(rootSecs), ui, pt, "Clear root directory", startTime, emuRate, true, systemRanges); err != nil && !errors.Is(err, retrodfrg.ErrInterrupted) {
						return err
					}
					ui.SetPhaseDone("root")
					updateStatusLines(ui, pt, startTime, "Clear root directory", emuRate, true, systemRanges)
					ui.LayoutAndDraw()
				}
				// Data area
				updateStatusLines(ui, pt, startTime, "Format data area", emuRate, true, systemRanges)
				ui.LayoutAndDraw()
				absData := int64(g.ReservedSectors) + int64(g.NumFATs)*int64(fatSecs)
				if ft != FAT32 {
					absData += int64(rootSecs)
				}
				remaining := int64(sz/512) - absData
				if remaining > 0 {
					_ = zeroSpanWithStatus(nw, absData, remaining, ui, pt, "Format data area", startTime, emuRate, true, systemRanges)
				}
				updateStatusLines(ui, pt, startTime, "Format complete", emuRate, true, systemRanges)
				ui.LayoutAndDraw()
				_ = waitWithStop(ui)
				ui.Close()

				printGeometryInfo(ft, sz, g, fatSecs, rootSecs, dataSecs, clusters, label, oem)
				fmt.Printf("\nFAT%d ready. bytes=%d emulate=true\n", ft, sz)
				return nil
			}

			// real write
			var sink io.WriterAt
			var file *os.File
			if out != "" {
				if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil && !errors.Is(err, os.ErrExist) {
					return err
				}
				f, err := os.Create(out)
				if err != nil {
					return err
				}
				file = f
				defer file.Close()
				if err := file.Truncate(sz); err != nil {
					return err
				}
				sink = file
			} else {
				// On Windows, use special API to open device with raw access flags
				var volHandle interface{} = nil
				var f *os.File
				var err error

				if runtime.GOOS == "windows" {
					// For drive letters like \\.\A:, lock and dismount the volume first
					h, prepErr := prepareWindowsDevice(device)
					if prepErr != nil {
						return fmt.Errorf("prepare device: %w", prepErr)
					}
					volHandle = h

					// Open the device path directly (don't try to map to PhysicalDrive - that doesn't work for USB floppies)
					f, err = openWindowsDevice(device)
				} else {
					// On Unix, use standard OpenFile
					f, err = os.OpenFile(device, os.O_RDWR, 0)
				}

				if err != nil {
					// Clean up volume handle if we have one
					if runtime.GOOS == "windows" && volHandle != nil {
						cleanupWindowsVolume(volHandle)
					}
					return fmt.Errorf("open device: %w", err)
				}
				file = f
				defer func() {
					file.Close()
					// Unlock and close volume handle after formatting
					if runtime.GOOS == "windows" && volHandle != nil {
						cleanupWindowsVolume(volHandle)
					}
				}()

				// Validate device size (best-effort). If unknown, proceed safely.
				deviceSize, err := getDeviceSize(f)
				if err != nil || deviceSize <= 0 {
					fmt.Fprintf(os.Stderr, "WARNING: cannot determine device size; proceeding without size check\n")
				} else {
					if deviceSize < sz {
						return fmt.Errorf("device too small: has %s, need %s", human(deviceSize), human(sz))
					}
					if deviceSize > sz {
						fmt.Fprintf(os.Stderr, "WARNING: device is %s, only formatting %s\n", human(deviceSize), human(sz))
					}
				}

				// Capability detection: if read sector 0 fails and --llf is set, attempt low-level format
				if attemptLLF {
					probe := make([]byte, 512)
					if _, err := file.ReadAt(probe, 0); err != nil {
						fmt.Fprintf(os.Stderr, "INFO: sector 0 not readable, attempting low-level format...\n")
						if err := tryLowLevelFormat(device, g); err != nil {
							return fmt.Errorf("low-level format not available: %w", err)
						}
						fmt.Fprintf(os.Stderr, "INFO: low-level format done. Continuing with filesystem build.\n")
					}
				}

				sink = file
			}

			ui.LayoutAndDraw()

			// Boot
			updateStatusLines(ui, pt, startTime, "Write boot sector", 0, false, systemRanges)
			ui.LayoutAndDraw()
			var boot []byte
			if ft == FAT32 {
				boot = buildBootSector32(g, label, oem)
			} else {
				boot = buildBootSector1216(ft, g, label, oem)
			}
			if err := writeSpanWithStatus(sink, 0, boot, ui, pt, "Write boot sector", startTime, 0, false, systemRanges); err != nil {
				return err
			}
			if file != nil {
				_ = file.Sync()
			}
			ui.SetPhaseDone("boot")
			updateStatusLines(ui, pt, startTime, "Write boot sector", 0, false, systemRanges)
			ui.LayoutAndDraw()

			// FSInfo + backup for FAT32
			if ft == FAT32 {
				updateStatusLines(ui, pt, startTime, "Write FSInfo", 0, false, systemRanges)
				ui.LayoutAndDraw()
				fsinfo := buildFSInfo()
				if err := writeSpanWithStatus(sink, int64(g.FSInfoSector), fsinfo, ui, pt, "Write FSInfo", startTime, 0, false, systemRanges); err != nil {
					return err
				}
				if file != nil {
					_ = file.Sync()
				}
				updateStatusLines(ui, pt, startTime, "Backup boot sector", 0, false, systemRanges)
				ui.LayoutAndDraw()
				if err := writeSpanWithStatus(sink, int64(g.BackupBootSector), boot, ui, pt, "Backup boot sector", startTime, 0, false, systemRanges); err != nil {
					return err
				}
				if file != nil {
					_ = file.Sync()
				}
			}

			// FAT #1
			updateStatusLines(ui, pt, startTime, "Initialize FAT #1", 0, false, systemRanges)
			ui.LayoutAndDraw()
			fatBytes := int64(fatSecs) * int64(g.BytesPerSector)
			fatBuf := make([]byte, fatBytes)
			if ft == FAT32 {
				initFAT32(fatBuf, g.Media)
			} else {
				initFAT1216(ft, fatBuf, g.Media)
			}
			if err := writeSpanWithStatus(sink, absFAT1, fatBuf, ui, pt, "Initialize FAT #1", startTime, 0, false, systemRanges); err != nil {
				return err
			}
			if file != nil {
				_ = file.Sync()
			}
			ui.SetPhaseDone("fat1")
			updateStatusLines(ui, pt, startTime, "Initialize FAT #1", 0, false, systemRanges)
			ui.LayoutAndDraw()

			// FAT #2
			updateStatusLines(ui, pt, startTime, "Duplicate FAT #2", 0, false, systemRanges)
			ui.LayoutAndDraw()
			if err := writeSpanWithStatus(sink, absFAT2, fatBuf, ui, pt, "Duplicate FAT #2", startTime, 0, false, systemRanges); err != nil {
				return err
			}
			if file != nil {
				_ = file.Sync()
			}
			ui.SetPhaseDone("fat2")
			updateStatusLines(ui, pt, startTime, "Duplicate FAT #2", 0, false, systemRanges)
			ui.LayoutAndDraw()

			// Root (1216)
			if ft != FAT32 {
				updateStatusLines(ui, pt, startTime, "Clear root directory", 0, false, systemRanges)
				ui.LayoutAndDraw()
				if err := zeroSpanWithStatus(sink, absRoot, int64(rootSecs), ui, pt, "Clear root directory", startTime, 0, false, systemRanges); err != nil {
					return err
				}
				if file != nil {
					_ = file.Sync()
				}
				if label != "" {
					entry := buildRootLabelEntry(label)
					if _, err := file.WriteAt(entry, (absRoot * 512)); err != nil {
						return err
					}
					ui.LayoutAndDraw()
				}
				ui.SetPhaseDone("root")
				updateStatusLines(ui, pt, startTime, "Clear root directory", 0, false, systemRanges)
				ui.LayoutAndDraw()
			} else {
				_ = zeroSpanWithStatus(sink, absData, 1, ui, pt, "Clear root directory", startTime, 0, false, systemRanges)
				if file != nil {
					_ = file.Sync()
				}
			}

			// Full format data area with sync policy
			if fullFormat {
				remainingSectors := int64(sz/512) - absData
				if remainingSectors > 0 {
					switch strings.ToLower(syncMode) {
					case "sector":
						updateStatusLines(ui, pt, startTime, "Full format (sector): zeroing data area", 0, false, systemRanges)
						ui.LayoutAndDraw()
						if err := fullFormatDataArea(file, absData, remainingSectors, ui, pt, "Full format (sector): zeroing data area", startTime, systemRanges); err != nil {
							fmt.Fprintf(os.Stderr, "\nWARNING: %v\n", err)
						}
					case "track", "phase", "none":
						updateStatusLines(ui, pt, startTime, "Full format (track): zeroing data area", 0, false, systemRanges)
						ui.LayoutAndDraw()
						if err := fullFormatTrack(file, absData, remainingSectors, int(g.SectorsPerTrack), ui, pt, syncMode, "Full format (track): zeroing data area", startTime, systemRanges); err != nil {
							fmt.Fprintf(os.Stderr, "\nWARNING: %v\n", err)
						}
						if verifyTrack {
							updateStatusLines(ui, pt, startTime, "Verify data area (track)", 0, false, systemRanges)
							ui.LayoutAndDraw()
							_ = verifyTrackRead(file, absData, remainingSectors, int(g.SectorsPerTrack))
						}
					}
				}
			}

			updateStatusLines(ui, pt, startTime, "Format complete", 0, false, systemRanges)
			ui.LayoutAndDraw()

			if err := waitWithStop(ui); err != nil && !errors.Is(err, retrodfrg.ErrInterrupted) {
				return err
			}
			ui.Close()

			printGeometryInfo(ft, sz, g, fatSecs, rootSecs, dataSecs, clusters, label, oem)

			total := uint32(0)
			if g.TotalSectors16 != 0 {
				total = uint32(g.TotalSectors16)
			} else {
				total = g.TotalSectors32
			}
			fmt.Printf("\nFAT%d ready. bytes=%d sectors=%d clusterSize=%dB clusters=%d fatSectors=%d rootDirSectors=%d dataSectors=%d emulate=false\n",
				ft, sz, total, int(g.SectorsPerCluster)*int(g.BytesPerSector), clusters, fatSecs, rootSecs, dataSecs)
			return nil
		},
	}

	// Format command flags
	formatCmd.Flags().StringVar(&ftStr, "type", "fat12", "fat12|fat16|fat32")
	formatCmd.Flags().StringVar(&sizeStr, "size", "", "total size (e.g. 360k, 720k, 1200k, 1440k, 32m, 2g)")
	_ = formatCmd.MarkFlagRequired("size")
	formatCmd.Flags().StringVar(&out, "out", "", "output image file path")
	formatCmd.Flags().StringVar(&device, "device", "", "block device path (e.g. /dev/fd0, /dev/sdb) [DANGEROUS]")
	formatCmd.Flags().BoolVar(&force, "force", false, "required with --device")
	formatCmd.Flags().StringVar(&label, "label", "", "volume label (<=11 ASCII)")
	formatCmd.Flags().StringVar(&oem, "oem", "EARMKFAT", "OEM string (<=8 ASCII)")
	formatCmd.Flags().IntVar(&heads, "heads", 0, "override number of heads")
	formatCmd.Flags().IntVar(&spt, "spt", 0, "override sectors per track")
	formatCmd.Flags().IntVar(&tracks, "tracks", 0, "override cylinders")
	formatCmd.Flags().BoolVar(&emulate, "emulate", false, "simulate floppy timing (no writes)")
	formatCmd.Flags().BoolVar(&fullFormat, "full", false, "full format: zero all data sectors and check for bad sectors")
	formatCmd.Flags().StringVar(&syncMode, "sync", "track", "sync policy: sector|track|phase|none")
	formatCmd.Flags().IntVar(&uiEvery, "ui-every", 64, "redraw UI every N sectors (REAL mode)")
	formatCmd.Flags().BoolVar(&verifyTrack, "verify", false, "verify one sector per track after formatting")
	formatCmd.Flags().BoolVar(&attemptLLF, "llf", false, "attempt low-level track format if device is not yet formatted")

	root.AddCommand(formatCmd)

	// Copy command for device/image backup and restore
	copyCmd := &cobra.Command{
		Use:   "copy",
		Short: "Copy data between devices and image files",
		Long:  "Copy data block-by-block from device to image (backup) or image to device (restore)",
	}

	// Device to image (backup)
	var (
		dev2imgDevice string
		dev2imgOut    string
		dev2imgForce  bool
		dev2imgBlock  int
	)
	copyToImage := &cobra.Command{
		Use:   "dev2img --device <device> --out <image>",
		Short: "Copy from device to image file (backup)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if dev2imgDevice == "" {
				return fmt.Errorf("--device is required")
			}
			if dev2imgOut == "" {
				return fmt.Errorf("--out is required")
			}
			if !dev2imgForce {
				return fmt.Errorf("--force is required for device operations")
			}

			return copyDeviceToImage(dev2imgDevice, dev2imgOut, int64(dev2imgBlock))
		},
	}
	copyToImage.Flags().StringVar(&dev2imgDevice, "device", "", "source block device (e.g. /dev/disk2)")
	copyToImage.Flags().StringVar(&dev2imgOut, "out", "", "output image file")
	copyToImage.Flags().BoolVar(&dev2imgForce, "force", false, "confirm device operation")
	copyToImage.Flags().IntVar(&dev2imgBlock, "block-size", 512, "block size for copying (bytes)")
	_ = copyToImage.MarkFlagRequired("device")
	_ = copyToImage.MarkFlagRequired("out")

	// Image to device (restore)
	var (
		img2devIn     string
		img2devDevice string
		img2devForce  bool
		img2devBlock  int
	)
	copyToDevice := &cobra.Command{
		Use:   "img2dev --in <image> --device <device>",
		Short: "Copy from image file to device (restore)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if img2devIn == "" {
				return fmt.Errorf("--in is required")
			}
			if img2devDevice == "" {
				return fmt.Errorf("--device is required")
			}
			if !img2devForce {
				return fmt.Errorf("--force is required for device operations")
			}

			return copyImageToDevice(img2devIn, img2devDevice, int64(img2devBlock))
		},
	}
	copyToDevice.Flags().StringVar(&img2devIn, "in", "", "source image file")
	copyToDevice.Flags().StringVar(&img2devDevice, "device", "", "target block device (e.g. /dev/disk2)")
	copyToDevice.Flags().BoolVar(&img2devForce, "force", false, "confirm device operation")
	copyToDevice.Flags().IntVar(&img2devBlock, "block-size", 512, "block size for copying (bytes)")
	_ = copyToDevice.MarkFlagRequired("in")
	_ = copyToDevice.MarkFlagRequired("device")

	copyCmd.AddCommand(copyToImage)
	copyCmd.AddCommand(copyToDevice)
	root.AddCommand(copyCmd)

	// Device discovery command (read-only; never formats)
	deviceCmd := &cobra.Command{
		Use:   "device",
		Short: "Device related utilities (safe, read-only)",
	}

	var listAll bool
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List compatible and non-compatible devices for formatting (read-only)",
		RunE: func(_ *cobra.Command, _ []string) error {
			infos, err := discoverDevices()
			if err != nil {
				return err
			}
			fmt.Printf("OS: %s\n", runtime.GOOS)
			fmt.Println("This is a SAFE, read-only listing. No formatting will be performed.")
			fmt.Println()
			fmt.Println("Compatible devices (usable with --device):")
			// Table header
			fmt.Printf("  %-18s  %-12s  %-20s  %-8s\n", "Path", "Type", "Serial", "Size")
			printedCompat := false
			for _, d := range infos {
				if !d.Compatible {
					continue
				}
				dtype, serial, sizeStr := getDeviceDetails(d.Path)
				fmt.Printf("  %-18s  %-12s  %-20s  %-8s\n", d.Path, dtype, serial, sizeStr)
				printedCompat = true
			}
			if !printedCompat {
				fmt.Println("  <none detected>")
			}
			fmt.Println()
			if listAll {
				fmt.Println("Non-compatible/partitions (will NOT be used with --device):")
				for _, d := range infos {
					if !d.Compatible {
						reason := d.Reason
						if strings.TrimSpace(reason) == "" {
							reason = "not a whole-disk device"
						}
						fmt.Printf("  %s  (%s)\n", d.Path, reason)
					}
				}
				fmt.Println()
			}
			if runtime.GOOS == "darwin" {
				mvs := listMountedDarwin()
				if len(mvs) > 0 {
					fmt.Println("Mounted volumes:")
					fmt.Printf("  %-24s  %-14s  %-18s  %-8s\n", "Mount", "FS", "Device", "Size")
					for _, m := range mvs {
						fmt.Printf("  %-24s  %-14s  %-18s  %-8s\n", m.MountPoint, m.FSType, m.Device, human(m.SizeBytes))
					}
					fmt.Println()
				}
			}
			if runtime.GOOS == "windows" {
				mvs := listMountedWindows()
				if len(mvs) > 0 {
					fmt.Println("Mounted volumes:")
					fmt.Printf("  %-24s  %-14s  %-18s  %-8s\n", "Mount", "Type", "Device", "Size")
					for _, m := range mvs {
						fmt.Printf("  %-24s  %-14s  %-18s  %-8s\n", m.MountPoint, m.FSType, m.Device, human(m.SizeBytes))
					}
					fmt.Println()
				}
			}
			fmt.Println("Notes:")
			switch runtime.GOOS {
			case "darwin":
				fmt.Println("  - Whole disks are typically /dev/diskN. Partitions like /dev/diskNsM are not compatible.")
			case "linux":
				fmt.Println("  - Whole disks: /dev/sdX, /dev/vdX, /dev/nvmeXnY, /dev/mmcblkX. Partitions (digits) are not compatible.")
			case "windows":
				fmt.Println("  - Raw device formatting of USB floppies is not supported on Windows. Use --out to create an image.")
			}
			return nil
		},
	}
	listCmd.Flags().BoolVar(&listAll, "all", false, "include non-compatible devices/partitions in output")

	deviceCmd.AddCommand(listCmd)

	// device info --path <mountpoint or device>
	var infoPath string
	infoCmd := &cobra.Command{
		Use:   "info",
		Short: "Show detailed info about a mount point or device (read-only)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(infoPath) == "" {
				return fmt.Errorf("--path is required")
			}
			dev, mnt, err := resolvePathToDevice(infoPath)
			if err != nil {
				return err
			}
			whole := dev
			// derive whole by trimming partition suffix for known patterns
			if runtime.GOOS == "darwin" {
				// /dev/(r)diskNsM -> /dev/(r)diskN
				b := filepath.Base(dev)
				for i := 0; i+1 < len(b); i++ {
					if b[i] == 's' && b[i+1] >= '0' && b[i+1] <= '9' {
						whole = filepath.Join("/dev", b[:i])
						break
					}
				}
			}
			if runtime.GOOS == "linux" {
				// sdXN -> sdX, nvmeXnYpZ -> nvmeXnY, mmcblkXpZ -> mmcblkX
				b := filepath.Base(dev)
				if isPartitionLinux(b) {
					// simplistic: trim trailing digits or 'p' + digits
					if idx := strings.LastIndexByte(b, 'p'); idx != -1 {
						whole = filepath.Join("/dev", b[:idx])
					} else {
						for len(b) > 0 && b[len(b)-1] >= '0' && b[len(b)-1] <= '9' {
							b = b[:len(b)-1]
						}
						whole = filepath.Join("/dev", b)
					}
				}
			}

			size := int64(-1)
			if f, err := os.Open(whole); err == nil {
				defer f.Close()
				size, _ = getDeviceSize(f)
			}

			fmt.Println("Path info")
			fmt.Printf("  Input:   %s\n", infoPath)
			fmt.Printf("  Device:  %s\n", dev)
			if mnt != "" {
				fmt.Printf("  Mounted: %s\n", mnt)
			}
			fmt.Printf("  Whole:   %s\n", whole)
			if size >= 0 {
				fmt.Printf("  Size:    %s\n", human(size))
			}
			// Heuristic media type
			if size > 0 {
				typ := mediaTypeBySize(size)
				if typ != "" {
					fmt.Printf("  Media:   %s\n", typ)
				}
			}
			return nil
		},
	}
	infoCmd.Flags().StringVar(&infoPath, "path", "", "mount point (e.g. /Volumes/XYZ) or device path (e.g. /dev/disk2)")
	_ = infoCmd.MarkFlagRequired("path")
	deviceCmd.AddCommand(infoCmd)
	root.AddCommand(deviceCmd)

	must(root.Execute())
}

// Device discovery (read-only)
type deviceInfo struct {
	Path       string
	Compatible bool
	Reason     string
}

func discoverDevices() ([]deviceInfo, error) {
	switch runtime.GOOS {
	case "darwin":
		return discoverDarwin()
	case "linux":
		return discoverLinux()
	case "windows":
		return discoverWindows()
	default:
		return nil, fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func discoverDarwin() ([]deviceInfo, error) {
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return nil, err
	}
	infos := []deviceInfo{}
	for _, e := range entries {
		name := e.Name()
		// Include both buffered and raw disk device nodes
		if strings.HasPrefix(name, "disk") || strings.HasPrefix(name, "rdisk") {
			path := filepath.Join("/dev", name)
			// Partition if there's an 's' immediately followed by a digit (e.g., disk2s1, rdisk3s2)
			isPart := false
			for i := 0; i+1 < len(name); i++ {
				if name[i] == 's' && name[i+1] >= '0' && name[i+1] <= '9' {
					isPart = true
					break
				}
			}
			if isPart {
				infos = append(infos, deviceInfo{Path: path, Compatible: false, Reason: "partition"})
			} else {
				infos = append(infos, deviceInfo{Path: path, Compatible: true})
			}
		}
	}
	return infos, nil
}

func discoverLinux() ([]deviceInfo, error) {
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return nil, err
	}
	infos := []deviceInfo{}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join("/dev", name)
		// Whole devices
		if isWholeLinuxDevice(name) {
			infos = append(infos, deviceInfo{Path: path, Compatible: true})
			continue
		}
		// Partitions / non-whole
		if isPartitionLinux(name) {
			infos = append(infos, deviceInfo{Path: path, Compatible: false, Reason: "partition"})
			continue
		}
		// Skip others, but show some notable types as non-compatible
		if strings.HasPrefix(name, "loop") {
			infos = append(infos, deviceInfo{Path: path, Compatible: false, Reason: "loop device"})
		}
	}
	return infos, nil
}

func isWholeLinuxDevice(name string) bool {
	// sdX, vdX
	if len(name) == 3 && (strings.HasPrefix(name, "sd") || strings.HasPrefix(name, "vd")) && name[2] >= 'a' && name[2] <= 'z' {
		return true
	}
	// nvmeXnY
	if strings.HasPrefix(name, "nvme") && strings.Contains(name, "n") && !strings.Contains(name, "p") {
		// e.g., nvme0n1
		parts := strings.Split(name, "n")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.Contains(parts[1], "p") {
			return true
		}
	}
	// mmcblkX
	if strings.HasPrefix(name, "mmcblk") && !strings.Contains(name, "p") {
		return true
	}
	return false
}

func isPartitionLinux(name string) bool {
	// sdXN or vdXN: trailing digit(s)
	if (strings.HasPrefix(name, "sd") || strings.HasPrefix(name, "vd")) && len(name) >= 4 {
		if name[len(name)-1] >= '0' && name[len(name)-1] <= '9' {
			return true
		}
	}
	// nvmeXnYpZ
	if strings.HasPrefix(name, "nvme") && strings.Contains(name, "n") && strings.Contains(name, "p") {
		return true
	}
	// mmcblkXpZ
	if strings.HasPrefix(name, "mmcblk") && strings.Contains(name, "p") {
		return true
	}
	return false
}

func discoverWindows() ([]deviceInfo, error) {
	// Probe a reasonable range for PhysicalDriveN
	infos := []deviceInfo{}
	for i := 0; i < 32; i++ {
		path := fmt.Sprintf("\\\\.\\PhysicalDrive%d", i)
		f, err := os.Open(path)
		if err == nil {
			_ = f.Close()
			infos = append(infos, deviceInfo{Path: path, Compatible: true})
		} else {
			// Still list as non-compatible if it exists but locked; we can't easily distinguish.
			// Only add a few common ones to avoid noise.
			if i < 8 {
				infos = append(infos, deviceInfo{Path: path, Compatible: false, Reason: "not accessible"})
			}
		}
	}
	return infos, nil
}

// Resolve a mount point or device path to its device and mount path
func resolvePathToDevice(p string) (device string, mountpoint string, err error) {
	p = filepath.Clean(p)
	// If path is already a device node
	if strings.HasPrefix(p, "/dev/") || strings.HasPrefix(p, `\\.\\`) {
		return p, findMountByDevice(p), nil
	}
	// Otherwise, treat as mountpoint. Try platform-specific resolution.
	switch runtime.GOOS {
	case "darwin":
		dev, mnt := findDarwinDeviceForMount(p)
		if dev == "" {
			return "", "", fmt.Errorf("cannot resolve device for %s", p)
		}
		return dev, mnt, nil
	case "linux":
		dev, mnt := findLinuxDeviceForMount(p)
		if dev == "" {
			return "", "", fmt.Errorf("cannot resolve device for %s", p)
		}
		return dev, mnt, nil
	case "windows":
		// Windows: user should pass \\.\PhysicalDriveN; mapping from mount to device is non-trivial without WMI
		return "", "", fmt.Errorf("on Windows, pass a device like \\.\\PhysicalDriveN with --path")
	default:
		return "", "", fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func findMountByDevice(_ string) string {
	switch runtime.GOOS {
	case "darwin":
		// Not implemented: would need a full scan mapping devices->mounts
	}
	return ""
}

func findLinuxDeviceForMount(target string) (device string, mountpoint string) {
	// Parse /proc/self/mounts
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return "", ""
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return "", ""
	}
	lines := strings.Split(string(b), "\n")
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		// format: <src> <target> <fstype> <opts> ...
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		src := fields[0]
		tgt := fields[1]
		if filepath.Clean(tgt) == filepath.Clean(target) {
			return src, tgt
		}
	}
	return "", ""
}

func bytesToString(b []int8) string {
	n := 0
	for n < len(b) && b[n] != 0 {
		n++
	}
	runes := make([]rune, n)
	for i := 0; i < n; i++ {
		runes[i] = rune(uint8(b[i]))
	}
	return string(runes)
}

func mediaTypeBySize(size int64) string {
	switch size {
	case 360 * 1024:
		return "360K floppy"
	case 720 * 1024:
		return "720K floppy"
	case 1200 * 1024:
		return "1.2M floppy"
	case 1440 * 1024:
		return "1.44M floppy"
	case 2880 * 1024:
		return "2.88M floppy"
	default:
		return ""
	}
}

// getDeviceDetails returns (type, serial, sizeHuman)
func getDeviceDetails(path string) (string, string, string) {
	// Default fallbacks
	dtype := "Disk"
	serial := "-"
	sizeStr := "-"
	var size int64 = -1

	// Type by platform
	switch runtime.GOOS {
	case "darwin":
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			if sz, err2 := getDeviceSize(f); err2 == nil {
				size = sz
				sizeStr = human(sz)
			}
		}
		// Classify floppy by canonical sizes
		switch size {
		case 360 * 1024, 720 * 1024, 1200 * 1024, 1440 * 1024, 2880 * 1024:
			dtype = "Floppy"
		default:
			dtype = "Disk"
		}
	case "linux":
		base := filepath.Base(path)
		// Derive sys block name (e.g., sda, nvme0n1)
		name := base
		// Read model/vendor/serial from /sys if present
		sysPath := filepath.Join("/sys/block", name)
		if _, err := os.Stat(sysPath); err != nil {
			// Some names appear under /sys/class/block
			sysPath = filepath.Join("/sys/class/block", name)
		}
		// Removable hint
		if b, err := os.ReadFile(filepath.Join(sysPath, "removable")); err == nil {
			if strings.TrimSpace(string(b)) == "1" {
				dtype = "Removable Disk"
			} else {
				dtype = "Fixed Disk"
			}
		}
		if b, err := os.ReadFile(filepath.Join(sysPath, "device", "serial")); err == nil {
			serial = strings.TrimSpace(string(b))
		}
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			if sz, err2 := getDeviceSize(f); err2 == nil {
				size = sz
				sizeStr = human(sz)
			}
		}
		switch size {
		case 360 * 1024, 720 * 1024, 1200 * 1024, 1440 * 1024, 2880 * 1024:
			dtype = "Floppy"
		}
	case "windows":
		dtype = "PhysicalDrive"
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			if sz, err2 := getDeviceSize(f); err2 == nil {
				size = sz
				sizeStr = human(sz)
			}
		}
		switch size {
		case 360 * 1024, 720 * 1024, 1200 * 1024, 1440 * 1024, 2880 * 1024:
			dtype = "Floppy"
		}
	}
	return dtype, serial, sizeStr
}

// Track-based zeroing with sync policy
func fullFormatTrack(file *os.File, absStart, sectors int64, spt int, ui *retrodfrg.UI, pt *progressTracker, syncMode string, currentOp string, startTime time.Time, systemRanges [][2]int64) error {
	if spt <= 0 {
		spt = 18
	}
	written := int64(0)
	for written < sectors {
		chunk := int64(spt)
		if sectors-written < chunk {
			chunk = sectors - written
		}
		if err := zeroSpanWithStatus(file, absStart+written, chunk, ui, pt, currentOp, startTime, 0, false, systemRanges); err != nil {
			return err
		}
		// Sync once per track unless disabled
		switch strings.ToLower(syncMode) {
		case "track", "phase":
			_ = file.Sync()
		case "none":
			// no sync here
		}
		written += chunk
	}
	// Phase-level final sync
	if strings.ToLower(syncMode) == "phase" {
		_ = file.Sync()
	}
	return nil
}

// Verify one sector per track (best-effort)
func verifyTrackRead(r io.ReaderAt, absStart, sectors int64, spt int) error {
	if spt <= 0 {
		spt = 18
	}
	buf := make([]byte, 512)
	for off := int64(0); off < sectors; off += int64(spt) {
		if _, err := r.ReadAt(buf, (absStart+off)*512); err != nil {
			return err
		}
	}
	return nil
}

// removed duplicate getDeviceDetails
