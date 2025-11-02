// Package retrodfrg provides a generic terminal UI for displaying progress and status information.
// It is designed to be completely agnostic of the underlying task being performed.
package retrodfrg

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"
)

// ErrInterrupted is returned when the user requests to stop the operation.
var ErrInterrupted = errors.New("interrupted")

// UI provides a terminal-based user interface for displaying customizable information.
// It supports title, summary lines, legend, phases, and status lines.
type UI struct {
	s        tcell.Screen
	stopChan chan struct{}
	once     sync.Once

	// Customizable display
	title        string
	phases       []string
	phaseDoneMap map[string]bool
	summaryLines []string
	legendLines  []string
	statusLines  []string

	// Visual progress map (provided by caller, UI just renders it)
	progressMapLines []string
}

// NewUI creates and initializes a new UI instance.
// It sets up the terminal screen and starts the event loop for handling user input.
func NewUI() (*UI, error) {
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
		stopChan:     make(chan struct{}),
		phaseDoneMap: make(map[string]bool),
	}
	go u.eventLoop()
	return u, nil
}

// Close closes the UI and restores the terminal to its original state.
func (u *UI) Close() {
	if u.s == nil {
		return
	}
	u.s.Fini()
	u.s = nil
	fmt.Print("\033[?1049l\033[?25h")
}

// RequestStop signals that the user has requested to stop the current operation.
// It can be called multiple times safely.
func (u *UI) RequestStop() {
	u.once.Do(func() {
		close(u.stopChan)
		u.s.PostEvent(tcell.NewEventInterrupt(nil))
	})
}

// IsStopped returns true if the user has requested to stop the operation.
func (u *UI) IsStopped() bool {
	select {
	case <-u.stopChan:
		return true
	default:
		return false
	}
}

// Size returns the current screen width and height.
func (u *UI) Size() (width, height int) {
	if u.s == nil {
		return 0, 0
	}
	return u.s.Size()
}

func putStr(s tcell.Screen, x, y int, str string) {
	w, _ := s.Size()
	runes := []rune(str)
	for i, r := range runes {
		pos := x + i
		if pos >= w {
			break // Don't write beyond screen width
		}
		s.SetContent(pos, y, r, nil, tcell.StyleDefault)
	}
}

// LayoutAndDraw redraws the entire UI with the current state.
// It should be called whenever the displayed information needs to be updated.
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

	// Progress map visualization (if provided)
	if len(u.progressMapLines) > 0 {
		// Compute available rows for progress map (leave room for phase+status: 7 lines)
		avail := h - currentY - 7
		if avail < 1 {
			avail = 1
		}
		rowsToShow := avail
		if rowsToShow > len(u.progressMapLines) {
			rowsToShow = len(u.progressMapLines)
		}
		for i := 0; i < rowsToShow && currentY < h; i++ {
			line := u.progressMapLines[i]
			// Truncate by rune count, not bytes
			runes := []rune(line)
			if len(runes) > w {
				runes = runes[:w]
			}
			putStr(u.s, 0, currentY, string(runes))
			currentY++
		}
	}

	// Phase line
	if len(u.phases) > 0 {
		putStr(u.s, 0, currentY, strings.Repeat("─", w))
		putStr(u.s, 2, currentY, " Phase ")
		currentY++
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
	if len(u.statusLines) > 0 {
		putStr(u.s, 0, currentY, strings.Repeat("─", w))
		putStr(u.s, 2, currentY, " Status ")
		currentY++
		for _, line := range u.statusLines {
			if currentY >= h {
				break
			}
			putStr(u.s, 0, currentY, line)
			currentY++
		}
	}

	u.s.Show()
}

// SetPhaseDone marks the specified phase as completed.
// The phase name is case-insensitive.
func (u *UI) SetPhaseDone(p string) {
	if u.phaseDoneMap == nil {
		u.phaseDoneMap = make(map[string]bool)
	}
	u.phaseDoneMap[strings.ToLower(p)] = true
}

// SetPhases sets the list of phases to display.
// Phases will be shown with checkmarks as they are marked done via SetPhaseDone.
func (u *UI) SetPhases(labels []string) {
	u.phases = append([]string(nil), labels...)
}

// SetTitle sets the title displayed at the top of the UI.
func (u *UI) SetTitle(t string) {
	u.title = t
}

// SetSummaryLines sets the summary/info lines displayed below the title.
func (u *UI) SetSummaryLines(lines []string) {
	u.summaryLines = append([]string(nil), lines...)
}

// SetLegend sets the legend lines displayed below the summary.
func (u *UI) SetLegend(lines []string) {
	u.legendLines = append([]string(nil), lines...)
}

// SetStatusLines sets the status lines displayed at the bottom of the UI.
func (u *UI) SetStatusLines(lines []string) {
	u.statusLines = append([]string(nil), lines...)
}

// SetProgressMap sets the visual progress map lines to display.
// Each string represents a row of the progress visualization.
// The UI simply renders what is provided - it does not track progress.
func (u *UI) SetProgressMap(lines []string) {
	u.progressMapLines = append([]string(nil), lines...)
}

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
