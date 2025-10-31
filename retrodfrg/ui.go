package retrodfrg

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
)

type FATType int

const (
	FAT12 FATType = 12
	FAT16 FATType = 16
	FAT32 FATType = 32
)

var ErrInterrupted = errors.New("interrupted")

type UI struct {
	s            tcell.Screen
	sectorMap    []bool
	start        time.Time
	currentOp    string
	drive        string
	bytesTotal   int64
	totalSectors int64
	written      int64
	curAbs       int64
	rateBps      float64

	full     rune
	free     rune
	sys      rune
	emulate  bool
	stopChan chan struct{}
	once     sync.Once

	// Customizable display
	title        string
	phases       []string
	phaseDoneMap map[string]bool
	systemRanges []struct{ start, end int64 } // inclusive
	summaryLines []string
	legendLines  []string
	statusLines  []string

	// Rendering throttle: update UI every N sectors when not emulating (>=1)
	updateEvery int

	// Sync policy for helpers: "sector" sync per sector; others skip per-sector
	syncMode string
}

func NewUI(_ FATType, drive string, bytesTotal int64, _ interface{}, _ ...uint32) (*UI, error) {
	s, err := tcell.NewScreen()
	if err != nil {
		return nil, err
	}
	if err := s.Init(); err != nil {
		return nil, err
	}
	s.DisableMouse()
	u := &UI{
		s:            s,
		drive:        drive,
		bytesTotal:   bytesTotal,
		totalSectors: int64(bytesTotal / 512),
		full:         '█',
		free:         '░',
		sys:          '■',
		start:        time.Now(),
		stopChan:     make(chan struct{}),
		phaseDoneMap: make(map[string]bool),
		updateEvery:  1,
		syncMode:     "sector",
	}
	u.sectorMap = make([]bool, u.totalSectors)
	go u.eventLoop()
	return u, nil
}

func (u *UI) Close() {
	if u.s == nil {
		return
	}
	u.s.Fini()
	u.s = nil
	fmt.Print("\033[?1049l\033[?25h")
}

func (u *UI) RequestStop() {
	u.once.Do(func() {
		close(u.stopChan)
		u.s.PostEvent(tcell.NewEventInterrupt(nil))
	})
}

func (u *UI) IsStopped() bool {
	select {
	case <-u.stopChan:
		return true
	default:
		return false
	}
}

func putStr(s tcell.Screen, x, y int, str string) {
	for i, r := range []rune(str) {
		s.SetContent(x+i, y, r, nil, tcell.StyleDefault)
	}
}

func (u *UI) LayoutAndDraw() {
	u.s.Clear()
	w, h := u.s.Size()

	currentY := 0

	// Title
	if u.title != "" {
		putStr(u.s, 0, currentY, strings.Repeat("═", w))
		centerX := (w - len(u.title)) / 2
		putStr(u.s, centerX, currentY, u.title)
		currentY++
	}

	// Optional summary/info lines
	for _, line := range u.summaryLines {
		if currentY >= h {
			break
		}
		putStr(u.s, 0, currentY, line)
		currentY++
	}

	// Optional legend
	for _, line := range u.legendLines {
		if currentY >= h {
			break
		}
		putStr(u.s, 0, currentY, line)
		currentY++
	}

	// Compute available rows for sector map (leave room for phase+status: 7 lines)
	avail := h - currentY - 7
	if avail < 1 {
		avail = 1
	}

	u.drawSectorMap(0, currentY, w, avail)
	currentY += avail

	// Phase line
	putStr(u.s, 0, currentY, strings.Repeat("─", w))
	putStr(u.s, 2, currentY, " Phase ")
	currentY++
	if len(u.phases) > 0 {
		check := func(ok bool) rune {
			if ok {
				return '✓'
			}
			return ' '
		}
		b := strings.Builder{}
		for i, p := range u.phases {
			if i > 0 {
				b.WriteByte(' ')
			}
			done := u.phaseDoneMap[strings.ToLower(p)]
			b.WriteString(fmt.Sprintf("[%c]%s", check(done), p))
		}
		putStr(u.s, 0, currentY, b.String())
		currentY++
	}

	// Status block
	putStr(u.s, 0, currentY, strings.Repeat("─", w))
	putStr(u.s, 2, currentY, " Status ")
	currentY++

	if len(u.statusLines) > 0 {
		for _, line := range u.statusLines {
			if currentY >= h {
				break
			}
			putStr(u.s, 0, currentY, line)
			currentY++
		}
	} else {
		mode := "REAL"
		if u.emulate {
			mode = "EMULATE"
		}
		el := time.Since(u.start).Truncate(time.Second)
		putStr(u.s, 0, currentY, fmt.Sprintf("Absolute: %06d", u.curAbs))
		currentY++
		putStr(u.s, 0, currentY, fmt.Sprintf("Written: %d / %d sectors", u.written, u.totalSectors))
		currentY++

		var rateBps float64
		if u.emulate {
			rateBps = u.rateBps
		} else {
			if elapsed := time.Since(u.start).Seconds(); elapsed > 0 {
				rateBps = float64(u.written*512) / elapsed
			}
		}
		// ETA
		var etaStr string
		if rateBps > 0 {
			remainBytes := (u.totalSectors - u.written) * 512
			eta := time.Duration(float64(remainBytes) / rateBps * float64(time.Second)).Truncate(time.Second)
			etaStr = eta.String()
		} else {
			etaStr = "—"
		}

		rateStr := human(int64(rateBps))
		putStr(u.s, 0, currentY, fmt.Sprintf("Elapsed: %s   Rate: %s/s   ETA: %s   Mode: %s", el, rateStr, etaStr, mode))
		currentY++
		putStr(u.s, 0, currentY, "Current op: "+u.currentOp)
	}

	u.s.Show()
}

func (u *UI) drawSectorMap(x, y, w, h int) {
	total := u.totalSectors
	if total <= 0 || h <= 0 {
		return
	}
	win := int64(w * h)
	start := int64(0)
	if total > win {
		if u.curAbs >= win-1 {
			start = u.curAbs - (win - 1)
		}
		if start+win > total {
			start = total - win
		}
	}

	sectorsToShow := total - start
	if sectorsToShow > win {
		sectorsToShow = win
	}

	inSystem := func(abs int64) bool {
		for _, r := range u.systemRanges {
			if abs >= r.start && abs <= r.end {
				return true
			}
		}
		return false
	}

	idx := int64(0)
	for row := 0; row < h; row++ {
		for col := 0; col < w; col++ {
			if idx >= sectorsToShow {
				return
			}
			abs := start + idx
			var rch = u.free
			if u.sectorMap[abs] {
				rch = u.full
			} else if inSystem(abs) {
				rch = u.sys
			}
			u.s.SetContent(x+col, y+row, rch, nil, tcell.StyleDefault)
			idx++
		}
	}
}

// API
func (u *UI) SetPhaseDone(p string) {
	if u.phaseDoneMap == nil {
		u.phaseDoneMap = make(map[string]bool)
	}
	u.phaseDoneMap[strings.ToLower(p)] = true
}
func (u *UI) SetPhases(labels []string) { u.phases = append([]string(nil), labels...) }
func (u *UI) SetTitle(t string)         { u.title = t }
func (u *UI) SetSystemRanges(ranges [][2]int64) {
	u.systemRanges = u.systemRanges[:0]
	for _, r := range ranges {
		u.systemRanges = append(u.systemRanges, struct{ start, end int64 }{start: r[0], end: r[1]})
	}
}
func (u *UI) AddSystemRange(start, end int64) {
	u.systemRanges = append(u.systemRanges, struct{ start, end int64 }{start: start, end: end})
}
func (u *UI) SetSummaryLines(lines []string) { u.summaryLines = append([]string(nil), lines...) }
func (u *UI) SetLegend(lines []string)       { u.legendLines = append([]string(nil), lines...) }
func (u *UI) SetStatusLines(lines []string)  { u.statusLines = append([]string(nil), lines...) }

func (u *UI) SetCurrentOp(op string) { u.currentOp = op }
func (u *UI) SetRate(bps float64)    { u.rateBps = bps }
func (u *UI) SetEmu(em bool)         { u.emulate = em }
func (u *UI) SetUpdateEvery(n int) {
	if n < 1 {
		n = 1
	}
	u.updateEvery = n
}
func (u *UI) SetSyncMode(m string) { u.syncMode = strings.ToLower(strings.TrimSpace(m)) }

func (u *UI) MarkRange(absStart int64, sectors int64) {
	end := absStart + sectors
	if end > u.totalSectors {
		end = u.totalSectors
	}
	for i := absStart; i < end; i++ {
		if !u.sectorMap[i] {
			u.sectorMap[i] = true
			u.written++
		}
	}
	if end-1 >= 0 {
		u.curAbs = end - 1
	}
}

func (u *UI) TotalSectors() int64 { return u.totalSectors }

func (u *UI) eventLoop() {
	go func() {
		for {
			select {
			case <-u.stopChan:
				return
			default:
			}
			ev := u.s.PollEvent()
			switch ev := ev.(type) {
			case *tcell.EventKey:
				switch {
				case ev.Key() == tcell.KeyCtrlC:
					u.RequestStop()
				case ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'Q'):
					u.RequestStop()
				case ev.Key() == tcell.KeyEscape:
					u.RequestStop()
				}
			case *tcell.EventResize:
				u.s.Sync()
			case *tcell.EventInterrupt:
				return
			case nil:
				return
			}
		}
	}()
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
